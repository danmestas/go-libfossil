package checkout

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// These tests cover where Checkout.Merge differs from fossil's merge, and
// its refusals; checkout_merge_interop_test.go compares it with fossil.

// mergeFixture is a repository with a base checkin and two children of it,
// local and merged, and a checkout of local.
type mergeFixture struct {
	repo   *repo.Repo
	co     *Checkout
	base   libfossil.FslID
	local  libfossil.FslID
	merged libfossil.FslID
}

// newMergeFixture commits base, then local and merged as children of base,
// and checks local out. Each map is the full file set of its checkin; a
// name ending in " +x" is committed executable under the name without it.
func newMergeFixture(t *testing.T, base, local, merged map[string]string) mergeFixture {
	t.Helper()
	r, err := repo.CreateWithEnv(t.TempDir()+"/merge.fossil", "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("close repo: %v", err)
		}
	})
	commit := func(files map[string]string, parent libfossil.FslID, day int) libfossil.FslID {
		var mf []manifest.File
		for name, content := range files {
			file := manifest.File{Name: name, Content: []byte(content)}
			if base, ok := strings.CutSuffix(name, " +x"); ok {
				file.Name, file.Perm = base, "x"
			}
			mf = append(mf, file)
		}
		sort.Slice(mf, func(i, j int) bool { return mf[i].Name < mf[j].Name })
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files: mf, Comment: "c", User: "test", Parent: parent,
			Time: time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	f := mergeFixture{repo: r}
	f.base = commit(base, 0, 1)
	f.merged = commit(merged, f.base, 2)
	f.local = commit(local, f.base, 3)

	f.co, err = Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.co.Close(); err != nil {
			t.Errorf("close checkout: %v", err)
		}
	})
	if v, _, err := f.co.Version(); err != nil || v != f.local {
		t.Fatalf("checkout at %d (%v), want local %d", v, err, f.local)
	}
	if err := f.co.Extract(f.local, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f mergeFixture) path(name string) string { return filepath.Join(f.co.Dir(), name) }

func (f mergeFixture) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(f.path(name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f mergeFixture) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(f.path(name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f mergeFixture) vmergeCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.co.db.QueryRow("SELECT count(*) FROM vmerge").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func actions(res MergeResult) []string {
	var out []string
	for _, file := range res.Files {
		out = append(out, file.Name+":"+actionName(file.Action))
	}
	return out
}

func actionName(a MergeAction) string {
	names := map[MergeAction]string{
		MergeUpdated: "updated", MergeMerged: "merged", MergeConflict: "conflict",
		MergeKept: "kept", MergeForked: "forked", MergeAdded: "added", MergeDeleted: "deleted",
		MergeMode: "mode",
	}
	return names[a]
}

// A file edited here but deleted in the merged-in version is kept: fossil
// deletes it and relies on undo, which go-libfossil does not have.
func TestMergeKeepsEditedFileDeletedThere(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "a\n", "keep.txt": "k\n"},
		map[string]string{"a.txt": "a\n", "keep.txt": "k\n"},
		map[string]string{"keep.txt": "k\n"})
	f.write(t, "a.txt", "a edited\n")

	res, err := f.co.Merge(MergeOpts{Version: f.merged})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(res); !reflect.DeepEqual(got, []string{"a.txt:kept"}) {
		t.Fatalf("actions = %q", got)
	}
	if got := f.read(t, "a.txt"); got != "a edited\n" {
		t.Fatalf("a.txt = %q, want the local edit kept", got)
	}
}

// A three-way merge reads the working file, so uncommitted edits survive.
func TestMergeKeepsUncommittedEdits(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"f.txt": "1\n2\n3\n4\n5\n"},
		map[string]string{"f.txt": "1\n2\n3\n4\n5\n"},
		map[string]string{"f.txt": "1m\n2\n3\n4\n5\n"})
	f.write(t, "f.txt", "1\n2\n3\n4\n5 local\n")

	res, err := f.co.Merge(MergeOpts{Version: f.merged})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(res); !reflect.DeepEqual(got, []string{"f.txt:merged"}) {
		t.Fatalf("actions = %q", got)
	}
	if got, want := f.read(t, "f.txt"), "1m\n2\n3\n4\n5 local\n"; got != want {
		t.Fatalf("f.txt = %q, want %q", got, want)
	}
}

// A merge whose added file would overwrite an untracked file is refused
// before anything changes.
func TestMergeRefusesToOverwriteUntrackedFile(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "a\n"},
		map[string]string{"a.txt": "a\n"},
		map[string]string{"a.txt": "a merged\n", "new.txt": "merged new\n"})
	f.write(t, "new.txt", "mine\n")

	_, err := f.co.Merge(MergeOpts{Version: f.merged})
	if err == nil || !strings.Contains(err.Error(), "untracked file new.txt") {
		t.Fatalf("Merge err = %v, want an untracked-file refusal", err)
	}
	if got := f.read(t, "new.txt"); got != "mine\n" {
		t.Fatalf("new.txt = %q, untracked file overwritten", got)
	}
	if got := f.read(t, "a.txt"); got != "a\n" {
		t.Fatalf("a.txt = %q, refused merge changed a file", got)
	}
	if n := f.vmergeCount(t); n != 0 {
		t.Fatalf("refused merge left %d vmerge rows", n)
	}
}

// A dry run reports the merge and changes nothing.
func TestMergeDryRunChangesNothing(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "a\n"},
		map[string]string{"a.txt": "a\n"},
		map[string]string{"a.txt": "a merged\n"})

	res, err := f.co.Merge(MergeOpts{Version: f.merged, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(res); !reflect.DeepEqual(got, []string{"a.txt:updated"}) {
		t.Fatalf("actions = %q", got)
	}
	if got := f.read(t, "a.txt"); got != "a\n" {
		t.Fatalf("dry run wrote a.txt = %q", got)
	}
	if n := f.vmergeCount(t); n != 0 {
		t.Fatalf("dry run left %d vmerge rows", n)
	}
}

// Merging an ancestor of the checkout, or the checkout itself, does
// nothing.
func TestMergeOfAncestorIsNoop(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "a\n"},
		map[string]string{"a.txt": "a local\n"},
		map[string]string{"a.txt": "a\n"})

	for _, v := range []libfossil.FslID{f.base, f.local} {
		res, err := f.co.Merge(MergeOpts{Version: v})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Files) != 0 {
			t.Fatalf("Merge(%d) files = %v, want none", v, res.Files)
		}
	}
	if n := f.vmergeCount(t); n != 0 {
		t.Fatalf("no-op merge left %d vmerge rows", n)
	}
}

// Merging the same version twice records it once.
func TestMergeTwiceRecordsOnce(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "1\n2\n3\n"},
		map[string]string{"a.txt": "1\n2\n3 local\n"},
		map[string]string{"a.txt": "1 merged\n2\n3\n"})

	for i := 0; i < 2; i++ {
		if _, err := f.co.Merge(MergeOpts{Version: f.merged}); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.vmergeCount(t); n != 2 {
		t.Fatalf("vmerge rows = %d, want 2 (the merge and a.txt)", n)
	}
}

// The conflict-fork strategy keeps the checkout's copy and records every
// version in the repository's conflict table.
func TestMergeConflictForkRecordsFork(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "base\n"},
		map[string]string{"a.txt": "local\n"},
		map[string]string{"a.txt": "merged\n"})

	res, err := f.co.Merge(MergeOpts{Version: f.merged, Strategy: "conflict-fork"})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(res); !reflect.DeepEqual(got, []string{"a.txt:forked"}) {
		t.Fatalf("actions = %q", got)
	}
	if got := f.read(t, "a.txt"); got != "local\n" {
		t.Fatalf("a.txt = %q, want the checkout's copy", got)
	}
	forks, err := merge.ListConflictForks(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forks, []string{"a.txt"}) {
		t.Fatalf("conflict forks = %q, want [a.txt]", forks)
	}
}

// An unknown strategy is refused before anything changes.
func TestMergeRefusesUnknownStrategy(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "base\n"},
		map[string]string{"a.txt": "local\n"},
		map[string]string{"a.txt": "merged\n"})

	if _, err := f.co.Merge(MergeOpts{Version: f.merged, Strategy: "nope"}); err == nil {
		t.Fatal("Merge with an unknown strategy succeeded")
	}
	if n := f.vmergeCount(t); n != 0 {
		t.Fatalf("refused merge left %d vmerge rows", n)
	}
}

// A merge's files are committed together: committing only some of them is
// refused, as fossil refuses it.
func TestCommitRefusesPartOfAMerge(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "a\n", "b.txt": "b\n"},
		map[string]string{"a.txt": "a local\n", "b.txt": "b\n"},
		map[string]string{"a.txt": "a\n", "b.txt": "b merged\n"})
	if _, err := f.co.Merge(MergeOpts{Version: f.merged}); err != nil {
		t.Fatal(err)
	}
	if err := f.co.Enqueue(EnqueueOpts{Paths: []string{"b.txt"}}); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.co.Commit(CommitOpts{Message: "part", User: "test"})
	if !errors.Is(err, errPartialMergeCommit) {
		t.Fatalf("Commit err = %v, want errPartialMergeCommit", err)
	}
}

// Committing a merge records the merge parent and clears vmerge.
func TestCommitRecordsMergeParent(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "a\n", "b.txt": "b\n"},
		map[string]string{"a.txt": "a local\n", "b.txt": "b\n"},
		map[string]string{"a.txt": "a\n", "b.txt": "b merged\n"})
	if _, err := f.co.Merge(MergeOpts{Version: f.merged}); err != nil {
		t.Fatal(err)
	}
	rid, _, err := f.co.Commit(CommitOpts{Message: "merge", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var pid int64
	err = f.repo.DB().QueryRow(
		"SELECT pid FROM plink WHERE cid=? AND isprim=0", int64(rid)).Scan(&pid)
	if err != nil {
		t.Fatalf("merge parent: %v", err)
	}
	if libfossil.FslID(pid) != f.merged {
		t.Fatalf("merge parent = %d, want %d", pid, f.merged)
	}
	if n := f.vmergeCount(t); n != 0 {
		t.Fatalf("commit left %d vmerge rows", n)
	}
}

// Committing a checkout that has no version and nothing added is refused
// with an error; it used to reach a panic in manifest.Checkin.
func TestCommitOfNothingIsAnError(t *testing.T) {
	r, cleanup := newTestEmptyRepo(t)
	defer cleanup()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := co.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	_, _, err = co.Commit(CommitOpts{Message: "empty", User: "test"})
	if !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("Commit err = %v, want ErrNothingToCommit", err)
	}
}

// A commit of a file that still holds conflict markers is refused, as
// fossil refuses it, unless AllowConflict is set.
func TestCommitRefusesConflictMarkers(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "base\n"},
		map[string]string{"a.txt": "local\n"},
		map[string]string{"a.txt": "merged\n"})
	res, err := f.co.Merge(MergeOpts{Version: f.merged})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(res); !reflect.DeepEqual(got, []string{"a.txt:conflict"}) {
		t.Fatalf("actions = %q", got)
	}

	_, _, err = f.co.Commit(CommitOpts{Message: "m", User: "test"})
	if !errors.Is(err, ErrUnresolvedConflicts) {
		t.Fatalf("Commit err = %v, want ErrUnresolvedConflicts", err)
	}
	if n := f.vmergeCount(t); n == 0 {
		t.Fatal("refused commit cleared the merge record")
	}
	if _, _, err := f.co.Commit(CommitOpts{
		Message: "m", User: "test", AllowConflict: true,
	}); err != nil {
		t.Fatalf("Commit with AllowConflict: %v", err)
	}
}

// A merge that would write where a directory stands is refused before any
// file is written, so no merged content is left without a merge record.
func TestMergeRefusesDirectoryInPlaceOfFile(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "1\n2\n3\n", "z.txt": "z\n"},
		map[string]string{"a.txt": "1\n2\n3 local\n", "z.txt": "z\n"},
		map[string]string{"a.txt": "1 merged\n2\n3\n", "z.txt": "z merged\n"})
	if err := os.Remove(f.path("z.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path("z.txt"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := f.co.Merge(MergeOpts{Version: f.merged})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Merge err = %v, want a refusal", err)
	}
	if got := f.read(t, "a.txt"); got != "1\n2\n3 local\n" {
		t.Fatalf("a.txt = %q, refused merge wrote a file", got)
	}
}

func (f mergeFixture) isExecutable(t *testing.T, name string) bool {
	t.Helper()
	info, err := os.Stat(f.path(name))
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()&0o100 != 0
}

// The merged-in version's executable bit is taken where the checkout did not
// change it: on a file whose content it left alone, and on a merged file.
func TestMergeTakesExecutableBit(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"run.sh": "echo\n", "lib.sh": "1\n2\n3\n"},
		map[string]string{"run.sh": "echo\n", "lib.sh": "1\n2\n3 local\n"},
		map[string]string{"run.sh +x": "echo\n", "lib.sh +x": "1 merged\n2\n3\n"})

	res, err := f.co.Merge(MergeOpts{Version: f.merged})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"lib.sh:merged", "run.sh:mode"}
	if got := actions(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	for _, name := range []string{"run.sh", "lib.sh"} {
		if !f.isExecutable(t, name) {
			t.Errorf("%s is not executable after the merge", name)
		}
		var isexe int
		err := f.co.db.QueryRow("SELECT isexe FROM vfile WHERE pathname=?", name).Scan(&isexe)
		if err != nil {
			t.Fatal(err)
		}
		if isexe != 1 {
			t.Errorf("%s: vfile.isexe = %d, want 1", name, isexe)
		}
	}
}

// A merge that would write under a path whose parent is a file is refused
// before any file is written.
func TestMergeRefusesFileInPlaceOfDirectory(t *testing.T) {
	f := newMergeFixture(t,
		map[string]string{"a.txt": "1\n2\n3\n"},
		map[string]string{"a.txt": "1\n2\n3 local\n"},
		map[string]string{"a.txt": "1 merged\n2\n3\n", "d/x.txt": "x\n"})
	f.write(t, "d", "a file where the merge needs a directory\n")

	_, err := f.co.Merge(MergeOpts{Version: f.merged})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Merge err = %v, want a refusal", err)
	}
	if got := f.read(t, "a.txt"); got != "1\n2\n3 local\n" {
		t.Fatalf("a.txt = %q, refused merge wrote a file", got)
	}
}
