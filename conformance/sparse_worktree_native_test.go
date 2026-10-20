//go:build !js

package conformance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/checkout"
)

func TestSparseWorktreeZitNativeBoundary(t *testing.T) {
	bin := zitBinary(t)
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("native Fossil is required for the boundary proof")
	}
	home, dir := t.TempDir(), t.TempDir()
	zitRun(t, bin, home, dir, "init")
	scWrite(t, dir, "src/a.c", "source\n")
	scWrite(t, dir, "tools/t.c", "outside\n")
	zitRun(t, bin, home, dir, "add", ".")
	zitRun(t, bin, home, dir, "commit", "--user", "u", "--no-sync", "-m", "baseline")
	zitRun(t, bin, home, dir, "update", "--cone=src")
	state, err := os.ReadFile(filepath.Join(dir, ".zit-checkout"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state) < 12 || string(state[:8]) != "ZITCKOT\x00" || state[8] != 4 || state[9] != 0 {
		t.Fatalf("reference checkout is not the pinned version4 container: %x", state)
	}
	for _, name := range []string{".fslckout", "_FOSSIL_"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("Zit created native checkout marker %s: %v", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"status"}, {"changes"}, {"commit", "--nosync", "-m", "must refuse"},
		{"update", "--nosync"}, {"revert"}, {"stash", "save"}, {"clean", "--force"},
	} {
		cmd := ref.command(ctx, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "within an open check-out") {
			t.Fatalf("native %v against Zit checkout: %v\n%s", args, err, out)
		}
	}
	after, err := os.ReadFile(filepath.Join(dir, ".zit-checkout"))
	if err != nil || string(after) != string(state) {
		t.Fatalf("native operations changed Zit state: %v", err)
	}
}

func TestSparseWorktreeGoCreatedNativeFixture(t *testing.T) {
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("native Fossil is required for the interoperability proof")
	}
	f := newSCRepo(t, false)
	info, err := os.Stat(f.r.Path())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	before, err := ref.command(ctx, "test-is-repo", f.r.Path()).CombinedOutput()
	if err != nil {
		t.Fatalf("native fixture probe: %v\n%s", err, before)
	}
	t.Logf("before checkpoint: main database size=%d; %s", info.Size(), before)
	if _, err := f.r.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	cmd := ref.command(ctx, "open", f.r.Path())
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native open of checkpointed Go-created fixture: %v\n%s", err, out)
	}
}

// A reduced vfile is not sufficient protection: Fossil synthesizes its next
// manifest from those rows and can silently retire omitted baseline files.
func TestSparseWorktreeNativeOmittedRecordHazard(t *testing.T) {
	ref := newFossilReference(t)
	if ref == nil {
		t.Skip("native Fossil is required for the interoperability proof")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		cmd := ref.command(ctx, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("native %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	repoPath := filepath.Join(t.TempDir(), "native.fossil")
	run("init", "--admin-user", "u", repoPath)
	run("open", repoPath)
	for name, text := range scFiles {
		scWrite(t, dir, name, text)
	}
	run("add", ".")
	run("commit", "--nosync", "--no-warnings", "-m", "baseline")
	ckdb, err := db.OpenSQL(filepath.Join(dir, checkout.PreferredDBName()), db.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ckdb.Exec("DELETE FROM vfile WHERE pathname NOT IN ('src/a.c', 'src/b.c', 'src/vendor/keep/k.c')"); err != nil {
		ckdb.Close()
		t.Fatal(err)
	}
	if err := ckdb.Close(); err != nil {
		t.Fatal(err)
	}
	scWrite(t, dir, "src/a.c", "native edit\n")
	run("commit", "--nosync", "--no-warnings", "-m", "native reduced record")
	out := run("ls")
	if strings.Contains(out, "docs/d.txt") {
		t.Fatalf("native Fossil unexpectedly preserved omitted baseline entry: %s", out)
	}
	t.Logf("Fossil silently committed only reduced managed record:\n%s", out)
}
