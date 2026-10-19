//go:build !js

package undo_test

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libdb "github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/undo"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// recordedMTime fails unless name's vfile.mtime is its file's mtime.
func recordedMTime(t *testing.T, ckout *sql.DB, ck, name string) {
	t.Helper()
	var mtime int64
	if err := ckout.QueryRow(
		"SELECT CAST(coalesce(mtime, 0) AS INTEGER) FROM vfile WHERE pathname=?", name,
	).Scan(&mtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(ck, name))
	if err != nil {
		t.Fatal(err)
	}
	if mtime != info.ModTime().Unix() {
		t.Fatalf("%s: vfile.mtime %d, file mtime %d", name, mtime, info.ModTime().Unix())
	}
}

func fossilIn(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// withCheckoutDB opens the checkout's database for fn and closes it before
// fossil runs again.
func withCheckoutDB(t *testing.T, ck string, fn func(ckout *sql.DB)) {
	t.Helper()
	// OpenSQL turns foreign keys off, as fossil runs and as checkouts open
	// this database: fossil's vfile references a blob table it does not have.
	ckout, err := libdb.OpenSQL(filepath.Join(ck, ".fslckout"), libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ckout.Close()
	fn(ckout)
}

// TestUndoVFileReadByFossil: on a checkout fossil scanned, undo here
// restores the file and the row fossil recorded for it, now with the written
// file's mtime, and redo the edit; fossil's own scan then reports what is on
// disk each time.
func TestUndoVFileReadByFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	// A clock an hour ahead makes each file written here recordable at once.
	env := &simio.Env{
		Clock:   simio.NewSimClockAt(time.Now().Add(time.Hour)),
		Rand:    simio.CryptoRand{},
		Storage: simio.OSStorage{},
	}
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "r.fossil")
	ck := filepath.Join(dir, "ck")
	if err := os.Mkdir(ck, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilIn(t, bin, dir, "init", repoPath)
	fossilIn(t, bin, ck, "open", repoPath)
	a := filepath.Join(ck, "a.txt")
	if err := os.WriteFile(a, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fossilIn(t, bin, ck, "add", "a.txt")
	fossilIn(t, bin, ck, "commit", "-m", "v1")
	if got := fossilIn(t, bin, ck, "changes"); got != "" {
		t.Fatalf("fossil changes after commit = %q", got)
	}

	withCheckoutDB(t, ck, func(ckout *sql.DB) {
		if err := undo.Save(ckout, env, ck, nil); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(a, []byte("hello world, edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := fossilIn(t, bin, ck, "changes"); got != "EDITED     a.txt" {
		t.Fatalf("fossil changes after edit = %q", got)
	}

	withCheckoutDB(t, ck, func(ckout *sql.DB) {
		if _, err := undo.Undo(ckout, env, ck); err != nil {
			t.Fatal(err)
		}
		recordedMTime(t, ckout, ck, "a.txt")
	})
	if got := fossilIn(t, bin, ck, "changes"); got != "" {
		t.Fatalf("fossil changes after undo = %q", got)
	}

	withCheckoutDB(t, ck, func(ckout *sql.DB) {
		if _, err := undo.Redo(ckout, env, ck); err != nil {
			t.Fatal(err)
		}
		recordedMTime(t, ckout, ck, "a.txt")
	})
	if got := fossilIn(t, bin, ck, "changes"); got != "EDITED     a.txt" {
		t.Fatalf("fossil changes after redo = %q", got)
	}
	fossilIn(t, bin, ck, "close", "--force")
}
