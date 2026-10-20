//go:build !js

package conformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/checkout"
)

func scWorktreeCompleteCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.4.4.record-only-merge", Req: "SC-4.4-1"}, scRecordOnlyMerge},
		{Case{ID: "sparsecone.4.4.record-only-conflict", Req: "SC-4.4-1"}, scRecordOnlyConflict},
	}
}

func TestSparseWorktreeComplete(t *testing.T) {
	for _, c := range scWorktreeCompleteCases() {
		t.Run(c.ID, func(t *testing.T) {
			if err := c.run(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func scRecordOnlyMerge(t *testing.T) error {
	for _, tree := range []bool{false, true} {
		f := newSCRepo(t, tree)
		remote := make(map[string]string, len(scFiles)+1)
		for name, text := range scFiles {
			remote[name] = text
		}
		remote["docs/d.txt"] = "remote docs\n"
		remote["tools/new.txt"] = "remote addition\n"
		delete(remote, "tools/t.c")
		remote["src/a.c"] = "remote source\n"
		mid := f.commit(t, remote, f.c1)
		co := f.open(t, scDefault(t), f.c1)
		if _, err := co.Merge(checkout.MergeOpts{Version: mid}); err != nil {
			return err
		}
		if got := scOnDisk(t, co.Dir()); !slices.Equal(got, scDefaultCovered) {
			return fmt.Errorf("merge materialized outside cone: %q", got)
		}
		rid, _, err := co.Commit(checkout.CommitOpts{Message: "merge", User: "u", Time: f.when.Add(time.Hour)})
		if err != nil {
			return err
		}
		got, err := scContent(f.r, rid)
		if err != nil {
			return err
		}
		if err := scSameContent("record-only merge", got, remote); err != nil {
			return err
		}
	}
	return nil
}

func scRecordOnlyConflict(t *testing.T) error {
	f := newSCRepo(t, false)
	files := func(text string) map[string]string {
		out := make(map[string]string, len(scFiles))
		for name, text := range scFiles {
			out[name] = text
		}
		out["docs/d.txt"] = text
		return out
	}
	ours := f.commit(t, files("ours\n"), f.c1)
	theirs := f.commit(t, files("theirs\n"), f.c1)
	co := f.open(t, scDefault(t), ours)
	if _, err := co.Merge(checkout.MergeOpts{Version: theirs}); err != nil {
		return err
	}
	if got := scOnDisk(t, co.Dir()); !slices.Equal(got, scDefaultCovered) {
		return fmt.Errorf("outside conflict materialized a file: %q", got)
	}
	conflicts, err := co.Conflicts()
	if err != nil || !slices.Equal(conflicts, []string{"docs/d.txt"}) {
		return fmt.Errorf("record-only conflicts = %q: %v", conflicts, err)
	}
	_, _, err = co.Commit(checkout.CommitOpts{Message: "unresolved", User: "u", Time: f.when.Add(time.Hour)})
	if !errors.Is(err, checkout.ErrUnresolvedConflicts) {
		return fmt.Errorf("record-only conflict commit: %v, want unresolved-conflict refusal", err)
	}
	wider, err := co.Cone().With([]string{"docs"}, nil)
	if err != nil {
		return err
	}
	if _, err := co.SetCone(wider); err != nil {
		return fmt.Errorf("widen to resolve record-only conflict: %w", err)
	}
	scWrite(t, co.Dir(), "docs/d.txt", "resolved\n")
	rid, _, err := co.Commit(checkout.CommitOpts{Message: "resolved", User: "u", Time: f.when.Add(2 * time.Hour)})
	if err != nil {
		return err
	}
	got, err := scContent(f.r, rid)
	if err != nil {
		return err
	}
	return scSameContent("resolved record-only conflict", got, files("resolved\n"))
}

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
