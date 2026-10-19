//go:build !js

package stash_test

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libdb "github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/stash"
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

// withDBs opens the checkout's and the repository's databases for fn and
// closes them before fossil runs again.
func withDBs(t *testing.T, ck, repoPath string, fn func(ckout, repoDB *sql.DB)) {
	t.Helper()
	// OpenSQL turns foreign keys off, as fossil runs and as checkouts open
	// these databases: fossil's vfile references a blob table the checkout
	// database does not have.
	ckout, err := libdb.OpenSQL(filepath.Join(ck, ".fslckout"), libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ckout.Close()
	repoDB, err := libdb.OpenSQL(repoPath, libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer repoDB.Close()
	fn(ckout, repoDB)
}

// TestStashVFileReadByFossil: after a stash save and pop made here, fossil's
// own scan of the checkout reports what is on disk: nothing after the save,
// which records the reverted file's mtime, and the edit after the pop, which
// records the row changed.
func TestStashVFileReadByFossil(t *testing.T) {
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
	// A different size: fossil itself misses a same-size edit in the second
	// its commit recorded.
	if err := os.WriteFile(a, []byte("hello world, edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := fossilIn(t, bin, ck, "changes"); got != "EDITED     a.txt" {
		t.Fatalf("fossil changes before save = %q", got)
	}

	withDBs(t, ck, repoPath, func(ckout, repoDB *sql.DB) {
		if err := stash.Save(ckout, repoDB, env, ck, "edit"); err != nil {
			t.Fatal(err)
		}
		recordedMTime(t, ckout, ck, "a.txt")
	})
	if got := fossilIn(t, bin, ck, "changes"); got != "" {
		t.Fatalf("fossil changes after save = %q", got)
	}

	withDBs(t, ck, repoPath, func(ckout, repoDB *sql.DB) {
		if err := stash.Pop(ckout, repoDB, env, ck); err != nil {
			t.Fatal(err)
		}
		recordedMTime(t, ckout, ck, "a.txt")
	})
	if got := fossilIn(t, bin, ck, "changes"); got != "EDITED     a.txt" {
		t.Fatalf("fossil changes after pop = %q", got)
	}
	fossilIn(t, bin, ck, "close", "--force")
}
