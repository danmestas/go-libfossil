//go:build !js

package stash_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	libdb "github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/stash"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// twinCheckouts is one repository with two checkouts: fossil works in ckF,
// go-libfossil in ckG.
type twinCheckouts struct {
	bin, repoPath, ckF, ckG string
	env                     *simio.Env
}

// newTwinCheckouts commits files to a new repository from ckF and opens ckG
// on the same version.
func newTwinCheckouts(t *testing.T, files map[string]string) twinCheckouts {
	t.Helper()
	tw := twinCheckouts{bin: testutil.RequireFossilBin(t)}
	tw.env = &simio.Env{
		Clock:   simio.NewSimClockAt(time.Now().Add(time.Hour)),
		Rand:    simio.CryptoRand{},
		Storage: simio.OSStorage{},
	}
	dir := t.TempDir()
	tw.repoPath = filepath.Join(dir, "r.fossil")
	tw.ckF, tw.ckG = filepath.Join(dir, "ckf"), filepath.Join(dir, "ckg")
	for _, ck := range []string{tw.ckF, tw.ckG} {
		if err := os.Mkdir(ck, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fossilIn(t, tw.bin, dir, "init", tw.repoPath)
	fossilIn(t, tw.bin, tw.ckF, "open", tw.repoPath)
	names := make([]string, 0, len(files))
	for name, body := range files {
		writeIn(t, tw.ckF, name, body)
		names = append(names, name)
	}
	fossilIn(t, tw.bin, tw.ckF, append([]string{"add"}, names...)...)
	fossilIn(t, tw.bin, tw.ckF, "commit", "-m", "base", "--no-warnings")
	fossilIn(t, tw.bin, tw.ckG, "open", tw.repoPath)
	return tw
}

// both writes files into both checkouts.
func (tw twinCheckouts) both(t *testing.T, files map[string]string) {
	t.Helper()
	for _, ck := range []string{tw.ckF, tw.ckG} {
		for name, body := range files {
			writeIn(t, ck, name, body)
		}
	}
}

// save stashes with fossil in ckF and with go-libfossil in ckG.
func (tw twinCheckouts) save(t *testing.T) {
	t.Helper()
	fossilIn(t, tw.bin, tw.ckF, "stash", "save", "-m", "s")
	withDBs(t, tw.ckG, tw.repoPath, tw.env, func(co *checkout.Checkout, ckout, repoDB *sql.DB) {
		if err := stash.Save(co, ckout, repoDB, tw.env, tw.ckG, "s"); err != nil {
			t.Fatal(err)
		}
	})
}

// pop pops with fossil in ckF and with go-libfossil in ckG, and returns the
// files each reported in conflict.
func (tw twinCheckouts) pop(t *testing.T) (fossilConflicts, goConflicts []string) {
	t.Helper()
	cmd := exec.Command(tw.bin, "stash", "pop")
	cmd.Dir = tw.ckF
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil stash pop: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if name, ok := strings.CutPrefix(line, "CONFLICT "); ok {
			fossilConflicts = append(fossilConflicts, name)
		}
	}
	withDBs(t, tw.ckG, tw.repoPath, tw.env, func(co *checkout.Checkout, ckout, repoDB *sql.DB) {
		if goConflicts, err = stash.Pop(ckout, repoDB, tw.env, tw.ckG); err != nil {
			t.Fatal(err)
		}
	})
	sort.Strings(fossilConflicts)
	sort.Strings(goConflicts)
	return fossilConflicts, goConflicts
}

// tree renders every file in ck but the checkout database: name, mode and
// bytes.
func tree(t *testing.T, ck string) string {
	t.Helper()
	entries, err := os.ReadDir(ck)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.Name() == ".fslckout" {
			continue
		}
		info, err := os.Lstat(filepath.Join(ck, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(ck, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %v %q\n", e.Name(), info.Mode().Perm(), data)
	}
	return b.String()
}

// requireSameTrees compares the working files and what fossil's scan reports.
func (tw twinCheckouts) requireSameTrees(t *testing.T, when string) {
	t.Helper()
	requireSame(t, when+": working files", tree(t, tw.ckF), tree(t, tw.ckG))
	requireSame(t, when+": fossil changes",
		fossilIn(t, tw.bin, tw.ckF, "changes"), fossilIn(t, tw.bin, tw.ckG, "changes"))
}

// Fossil's stash_apply merges a stashed edit three ways onto a file changed
// since the stash was taken (pivot: the stash's baseline; mine: the file on
// disk; theirs: the stashed content): a clean merge keeps both changes, an
// overlap is written with fossil's conflict markers and the three inputs
// kept beside it as NAME-baseline, NAME-original and NAME-merge, and binary
// content cannot be merged, so the file is left empty. Each conflict is
// reported; the pop still succeeds and drops the stash.
func TestStashApplyMergesChangedFilesMatchesFossil(t *testing.T) {
	tw := newTwinCheckouts(t, map[string]string{
		"m.txt": "1\n2\n3\n4\n5\n",
		"n.txt": "x\n",
		"b.bin": "\x00bin\n",
	})
	tw.both(t, map[string]string{
		"m.txt": "1\nSS\n3\n4\n5\n",
		"n.txt": "stashed\n",
		"b.bin": "\x00binstash\n",
	})
	tw.save(t)
	tw.requireSameTrees(t, "after save")
	tw.both(t, map[string]string{
		"m.txt": "1\n2\n3\n4\nCCC\n",
		"n.txt": "current!\n",
		"b.bin": "\x00bincurrent\n",
	})
	fc, gc := tw.pop(t)
	requireSame(t, "conflicts", strings.Join(fc, "\n"), strings.Join(gc, "\n"))
	if !slices.Equal(fc, []string{"b.bin", "n.txt"}) {
		t.Fatalf("fossil conflicts = %q", fc)
	}
	tw.requireSameTrees(t, "after pop")
	requireSame(t, "stash", query(t, tw.ckF, stashRows), query(t, tw.ckG, stashRows))
}

// A stash popped after an update to a version that changed the file merges
// the stashed edit onto the updated file, as fossil's does.
func TestStashApplyAfterUpdateMatchesFossil(t *testing.T) {
	tw := newTwinCheckouts(t, map[string]string{"m.txt": "1\n2\n3\n4\n5\n"})
	base := artifactOf(t, tw.bin, tw.ckF, "trunk")
	writeIn(t, tw.ckF, "m.txt", "1\n2\n3\n4\nNEXT\n")
	fossilIn(t, tw.bin, tw.ckF, "commit", "-m", "next", "--no-warnings")
	fossilIn(t, tw.bin, tw.ckF, "update", base)
	tw.both(t, map[string]string{"m.txt": "1\nSTASHED\n3\n4\n5\n"})
	tw.save(t)
	for _, ck := range []string{tw.ckF, tw.ckG} {
		fossilIn(t, tw.bin, ck, "update", "trunk")
	}
	fc, gc := tw.pop(t)
	if len(fc) != 0 || len(gc) != 0 {
		t.Fatalf("conflicts: fossil %q, go-libfossil %q", fc, gc)
	}
	tw.requireSameTrees(t, "after pop")
	if got := tree(t, tw.ckG); !strings.Contains(got, `"1\nSTASHED\n3\n4\nNEXT\n"`) {
		t.Fatalf("pop after update lost a change:\n%s", got)
	}
}

// A merge that changes only a file's executable bit leaves the row's isexe
// set with no other change. Stashing an edit to that file reverts it as
// fossil's revert does, with the checked-out version's bit (and isexe), and
// records the merged bit, which the pop restores.
func TestStashMergedExecBitMatchesFossil(t *testing.T) {
	tw := newTwinCheckouts(t, map[string]string{"a.sh": "a\n", "b.txt": "b\n"})
	fossilIn(t, tw.bin, tw.ckF, "branch", "new", "br", "trunk")
	fossilIn(t, tw.bin, tw.ckF, "update", "br")
	if err := os.Chmod(filepath.Join(tw.ckF, "a.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	fossilIn(t, tw.bin, tw.ckF, "commit", "-m", "exec on br", "--no-warnings")
	fossilIn(t, tw.bin, tw.ckF, "update", "trunk")
	writeIn(t, tw.ckF, "b.txt", "trunk b\n")
	fossilIn(t, tw.bin, tw.ckF, "commit", "-m", "on trunk", "--no-warnings")
	fossilIn(t, tw.bin, tw.ckG, "update", "trunk")
	for _, ck := range []string{tw.ckF, tw.ckG} {
		fossilIn(t, tw.bin, ck, "merge", "br")
	}
	tw.both(t, map[string]string{"a.sh": "a edited\n"})
	const rows = "SELECT pathname, rid, mrid, chnged, isexe FROM vfile ORDER BY pathname"
	const stashExec = "SELECT newname, CAST(isExec AS INT) FROM stashfile ORDER BY newname"
	tw.save(t)
	requireSame(t, "vfile after save", query(t, tw.ckF, rows), query(t, tw.ckG, rows))
	requireSame(t, "stash after save", query(t, tw.ckF, stashExec), query(t, tw.ckG, stashExec))
	tw.requireSameTrees(t, "after save")
	tw.pop(t)
	tw.requireSameTrees(t, "after pop")
	requireSame(t, "vfile after pop and scan", query(t, tw.ckF, rows), query(t, tw.ckG, rows))
	if got := tree(t, tw.ckG); !strings.Contains(got, "a.sh -rwxr-xr-x") {
		t.Fatalf("pop lost the merged executable bit:\n%s", got)
	}
}

// In a sparse checkout, a merge that changes only executable bits is
// recorded for a file outside the cone without writing it (sparse_pending),
// and applied to vfile for one inside. Stashing an edit to the file inside
// reverts it with the checked-out version's bit and the pop restores the
// merged one, as fossil's do; the record outside the cone is left as it is.
func TestStashMergedExecBitSparse(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
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
	for _, sub := range []string{"src", "tools"} {
		if err := os.Mkdir(filepath.Join(ck, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeIn(t, ck, "src/x.sh", "x\n")
	writeIn(t, ck, "src/a.c", "a\n")
	writeIn(t, ck, "tools/t.sh", "t\n")
	fossilIn(t, bin, ck, "add", "src/x.sh", "src/a.c", "tools/t.sh")
	fossilIn(t, bin, ck, "commit", "-m", "base", "--no-warnings")
	fossilIn(t, bin, ck, "branch", "new", "br", "trunk")
	fossilIn(t, bin, ck, "update", "br")
	for _, name := range []string{"src/x.sh", "tools/t.sh"} {
		if err := os.Chmod(filepath.Join(ck, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fossilIn(t, bin, ck, "commit", "-m", "exec on br", "--no-warnings")
	fossilIn(t, bin, ck, "update", "trunk")
	writeIn(t, ck, "src/a.c", "trunk a\n")
	fossilIn(t, bin, ck, "commit", "-m", "on trunk", "--no-warnings")
	trunkUUID, brUUID := artifactOf(t, bin, ck, "trunk"), artifactOf(t, bin, ck, "br")
	fossilIn(t, bin, ck, "close")
	if err := os.RemoveAll(ck); err != nil {
		t.Fatal(err)
	}

	r, err := repo.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var trunk, br libfossil.FslID
	for uuid, rid := range map[string]*libfossil.FslID{trunkUUID: &trunk, brUUID: &br} {
		if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(rid); err != nil {
			t.Fatal(err)
		}
	}
	src, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	co, err := checkout.Create(r, ck, checkout.CreateOpts{Env: env, Cone: src})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if err := co.Extract(trunk, checkout.ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Merge(checkout.MergeOpts{Version: br}); err != nil {
		t.Fatal(err)
	}
	writeIn(t, ck, "src/x.sh", "x edited\n")
	if err := os.Chmod(filepath.Join(ck, "src/x.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	ckName, err := checkout.FindCheckoutDB(simio.OSStorage{}, ck, false)
	if err != nil {
		t.Fatal(err)
	}
	ckout, err := libdb.OpenSQL(ckName, libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ckout.Close()
	const pending = "SELECT pathname, rid, mrid, chnged, deleted, isexe FROM sparse_pending ORDER BY pathname"
	const rows = "SELECT pathname, chnged, isexe FROM vfile ORDER BY pathname"
	before := query(t, ck, pending)
	if !strings.Contains(before, "tools/t.sh") {
		t.Fatalf("merge recorded no out-of-cone outcome:\n%s", before)
	}
	if got := query(t, ck, rows); !strings.Contains(got, "src/x.sh|0|1\n") {
		t.Fatalf("merge did not set src/x.sh's bit alone:\n%s", got)
	}
	if err := stash.Save(co, ckout, r.DB().SqlDB(), env, ck, "sparse"); err != nil {
		t.Fatal(err)
	}
	requireMode(t, filepath.Join(ck, "src/x.sh"), 0o644)
	if got := query(t, ck, rows); !strings.Contains(got, "src/x.sh|0|0\n") {
		t.Fatalf("save left the merged bit on src/x.sh's row:\n%s", got)
	}
	requireSame(t, "record-only merge after save", before, query(t, ck, pending))
	if _, err := stash.Pop(ckout, r.DB().SqlDB(), env, ck); err != nil {
		t.Fatal(err)
	}
	requireMode(t, filepath.Join(ck, "src/x.sh"), 0o755)
	requireSame(t, "record-only merge after pop", before, query(t, ck, pending))
	if _, err := os.Lstat(filepath.Join(ck, "tools")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stash wrote outside the cone: %v", err)
	}
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s: mode %v, want %v", path, got, want)
	}
}
