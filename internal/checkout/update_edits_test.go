package checkout

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// editsRepo holds version A and its child B:
//
//	merge.txt  A "a\nb\nc\n"   B "A\nb\nc\n"   (B edits the first line)
//	clash.txt  A "x\n"         B "y\n"
//	gone.txt   A "gone\n"      B (deleted)
//	plain.txt  A "plain\n"     B "plain, B\n"
//	same.txt   A "same\n"      B "same\n"
func editsRepo(t *testing.T) (*repo.Repo, libfossil.FslID, libfossil.FslID) {
	t.Helper()
	r, err := repo.CreateWithEnv(filepath.Join(t.TempDir(), "e.fossil"), "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	checkin := func(parent libfossil.FslID, day int, files map[string]string) libfossil.FslID {
		var fs []manifest.File
		for name, body := range files {
			fs = append(fs, manifest.File{Name: name, Content: []byte(body)})
		}
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files: fs, Comment: "v", User: "test", Parent: parent,
			Time: time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	a := checkin(0, 1, map[string]string{
		"merge.txt": "a\nb\nc\n", "clash.txt": "x\n", "gone.txt": "gone\n",
		"plain.txt": "plain\n", "same.txt": "same\n",
	})
	b := checkin(a, 2, map[string]string{
		"merge.txt": "A\nb\nc\n", "clash.txt": "y\n",
		"plain.txt": "plain, B\n", "same.txt": "same\n",
	})
	return r, a, b
}

// editsCheckout checks out version a into a fresh directory.
func editsCheckout(t *testing.T, r *repo.Repo, a libfossil.FslID) *Checkout {
	t.Helper()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	if err := co.Extract(a, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	return co
}

func writeWorking(t *testing.T, co *Checkout, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(co.dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readWorking(t *testing.T, co *Checkout, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(co.dir, name))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

func tracked(t *testing.T, co *Checkout, name string) bool {
	t.Helper()
	vid, _, _ := co.Version()
	var n int
	if err := co.db.QueryRow("SELECT count(*) FROM vfile WHERE vid=? AND pathname=?",
		int64(vid), name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// A local edit and the target's edit to other lines of a file are merged,
// with the working file as the local side.
func TestUpdateMergesLocalEdit(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "merge.txt", "a\nb\nC\n")

	res, err := co.Update(UpdateOpts{TargetRID: b})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "merge.txt"); got != "A\nb\nC\n" {
		t.Fatalf("merge.txt = %q, want both edits", got)
	}
	if len(res.Conflicted) != 0 {
		t.Fatalf("Conflicted = %v, want none", res.Conflicted)
	}
	if got := statusOf(t, co); got["merge.txt"] != ChangeModified {
		t.Fatalf("status = %v, want merge.txt modified against the target", got)
	}
}

// A local edit to a file the target deletes is kept on disk, no longer
// tracked, as fossil's update does.
func TestUpdateKeepsEditedFileTargetDeletes(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "gone.txt", "gone, edited\n")

	res, err := co.Update(UpdateOpts{TargetRID: b})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := readWorking(t, co, "gone.txt"); !ok || got != "gone, edited\n" {
		t.Fatalf("gone.txt = %q (present %v), want the local edit kept", got, ok)
	}
	if containsPath(res.FilesRemoved, "gone.txt") {
		t.Fatalf("FilesRemoved = %v, must not list the kept file", res.FilesRemoved)
	}
	if tracked(t, co, "gone.txt") {
		t.Fatal("gone.txt still tracked; fossil leaves it unmanaged")
	}
}

// A local edit that clashes with the target's gives conflict markers and a
// Conflicted entry.
func TestUpdateConflictingLocalEdit(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "clash.txt", "z\n")

	res, err := co.Update(UpdateOpts{TargetRID: b})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readWorking(t, co, "clash.txt")
	if !strings.Contains(got, "<<<<<<<") || !strings.Contains(got, "z\n") || !strings.Contains(got, "y\n") {
		t.Fatalf("clash.txt = %q, want markers around both edits", got)
	}
	if len(res.Conflicted) != 1 || res.Conflicted[0] != "clash.txt" {
		t.Fatalf("Conflicted = %v, want [clash.txt]", res.Conflicted)
	}
}

// Files without local changes take the target's content, and one the target
// deletes is removed, as before.
func TestUpdateCleanFilesFastForward(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)

	res, err := co.Update(UpdateOpts{TargetRID: b})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "plain.txt"); got != "plain, B\n" {
		t.Fatalf("plain.txt = %q, want the target's", got)
	}
	if _, ok := readWorking(t, co, "gone.txt"); ok {
		t.Fatal("gone.txt still on disk")
	}
	if !containsPath(res.FilesRemoved, "gone.txt") || len(res.Conflicted) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status = %v, want clean", got)
	}
}

// A pending add stays added and a pending removal stays removed.
func TestUpdateKeepsPendingAddAndRemove(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "new.txt", "new\n")
	if _, err := co.Manage(ManageOpts{Paths: []string{"new.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := co.Unmanage(UnmanageOpts{Paths: []string{"same.txt"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := co.Update(UpdateOpts{TargetRID: b}); err != nil {
		t.Fatal(err)
	}
	got := statusOf(t, co)
	if got["new.txt"] != ChangeAdded || got["same.txt"] != ChangeRemoved {
		t.Fatalf("status = %v, want new.txt added and same.txt removed", got)
	}
	if body, _ := readWorking(t, co, "new.txt"); body != "new\n" {
		t.Fatalf("new.txt = %q", body)
	}
}

// A dry run reports the merge without writing anything.
func TestUpdateDryRunWithLocalEdit(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "clash.txt", "z\n")

	res, err := co.Update(UpdateOpts{TargetRID: b, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "clash.txt"); got != "z\n" {
		t.Fatalf("dry run wrote clash.txt: %q", got)
	}
	if got, _ := readWorking(t, co, "plain.txt"); got != "plain\n" {
		t.Fatalf("dry run wrote plain.txt: %q", got)
	}
	if len(res.Conflicted) != 1 || !containsPath(res.FilesWritten, "plain.txt") {
		t.Fatalf("dry run result = %+v, want the conflict and the write reported", res)
	}
	if vid, _, _ := co.Version(); vid != a {
		t.Fatalf("dry run moved the checkout to %d", vid)
	}
}

// Without local changes, an update to a version on another branch takes
// that version's content, as fossil's update does: it merges only
// uncommitted edits, never the two versions' committed differences.
func TestUpdateToSiblingTakesTargetContent(t *testing.T) {
	r, a, b := editsRepo(t)
	sibling, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "merge.txt", Content: []byte("a\nb\nsibling\n")},
			{Name: "clash.txt", Content: []byte("x\n")},
		},
		Comment: "sibling", User: "test", Parent: a,
		Time: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	co := editsCheckout(t, r, b) // B and sibling are both children of A
	res, err := co.Update(UpdateOpts{TargetRID: sibling})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "merge.txt"); got != "a\nb\nsibling\n" {
		t.Fatalf("merge.txt = %q, want the target's", got)
	}
	if len(res.Conflicted) != 0 {
		t.Fatalf("Conflicted = %v, want none", res.Conflicted)
	}
	if got := statusOf(t, co); len(got) != 0 {
		t.Fatalf("status = %v, want clean", got)
	}
}

// forkRepo commits base, then two children of it, each with its own files;
// with no files for the second, it has one child.
func forkRepo(t *testing.T, base, left, right []manifest.File) (*repo.Repo, libfossil.FslID, libfossil.FslID, libfossil.FslID) {
	t.Helper()
	r, err := repo.CreateWithEnv(filepath.Join(t.TempDir(), "f.fossil"), "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	checkin := func(parent libfossil.FslID, day int, files []manifest.File) libfossil.FslID {
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files: files, Comment: "v", User: "test", Parent: parent,
			Time: time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	b := checkin(0, 1, base)
	l := checkin(b, 2, left)
	if len(right) == 0 {
		return r, b, l, 0
	}
	return r, b, l, checkin(b, 3, right)
}

// A binary file both the working copy and the target changed is not line
// merged: fossil copies the working file to NAME-original, writes the
// target's content and counts a conflict.
func TestUpdateBinaryConflictBacksUpLocal(t *testing.T) {
	r, a, b, _ := forkRepo(t,
		[]manifest.File{{Name: "bin.dat", Content: []byte("a\x00b\nc\n")}},
		[]manifest.File{{Name: "bin.dat", Content: []byte("A\x00b\nc\n")}},
		nil)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "bin.dat", "a\x00b\nC\n")

	res, err := co.Update(UpdateOpts{TargetRID: b})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "bin.dat"); got != "A\x00b\nc\n" {
		t.Fatalf("bin.dat = %q, want the target's", got)
	}
	if got, _ := readWorking(t, co, "bin.dat-original"); got != "a\x00b\nC\n" {
		t.Fatalf("bin.dat-original = %q, want the local copy", got)
	}
	if len(res.Conflicted) != 0 || strings.Join(res.Unmerged, ",") != "bin.dat" ||
		!containsPath(res.FilesWritten, "bin.dat") {
		t.Fatalf("result = %+v, want bin.dat written and unmerged", res)
	}
}

// Merge keeps the checkout's copy of a binary file both sides changed, as
// fossil's merge does ("cannot merge binary file"), whatever strategy the
// file would otherwise get.
func TestMergeKeepsBinaryFile(t *testing.T) {
	r, _, left, right := forkRepo(t,
		[]manifest.File{{Name: "bin.dat", Content: []byte("a\x00b\nc\n")}},
		[]manifest.File{{Name: "bin.dat", Content: []byte("A\x00b\nc\n")}},
		[]manifest.File{{Name: "bin.dat", Content: []byte("a\x00b\nC\n")}})
	co := editsCheckout(t, r, left)

	res, err := co.Merge(MergeOpts{Version: right})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "bin.dat"); got != "A\x00b\nc\n" {
		t.Fatalf("bin.dat = %q, want the checkout's copy kept", got)
	}
	if len(res.Files) != 1 || res.Files[0].Action != MergeKept {
		t.Fatalf("files = %+v, want bin.dat kept", res.Files)
	}
}

// Files left to reconcile without markers are listed in Unmerged: an edited
// file the target deletes, and a file added both locally and by the target.
func TestUpdateListsUnmerged(t *testing.T) {
	r, a, b, _ := forkRepo(t,
		[]manifest.File{{Name: "gone.txt", Content: []byte("gone\n")}},
		[]manifest.File{{Name: "both.txt", Content: []byte("theirs\n")}},
		nil)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "gone.txt", "gone, edited\n")
	writeWorking(t, co, "both.txt", "mine\n")
	if _, err := co.Manage(ManageOpts{Paths: []string{"both.txt"}}); err != nil {
		t.Fatal(err)
	}

	res, err := co.Update(UpdateOpts{TargetRID: b})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Unmerged, ","); got != "both.txt,gone.txt" {
		t.Fatalf("Unmerged = %q, want both.txt,gone.txt", got)
	}
	if got, _ := readWorking(t, co, "both.txt"); got != "mine\n" {
		t.Fatalf("both.txt = %q, want the local file kept", got)
	}
}

// An update that cannot finish is refused before anything is written: here
// the target adds a file where an untracked directory stands.
func TestUpdateRefusesBeforeWriting(t *testing.T) {
	r, a, b := editsRepo(t)
	sub, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "merge.txt", Content: []byte("A\nb\nc\n")},
			{Name: "plain.txt", Content: []byte("plain, B\n")},
			{Name: "dir.txt", Content: []byte("file\n")},
		},
		Comment: "adds dir.txt", User: "test", Parent: b,
		Time: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "merge.txt", "a\nb\nC\n")
	if err := os.Mkdir(filepath.Join(co.dir, "dir.txt"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := co.Update(UpdateOpts{TargetRID: sub}); err == nil {
		t.Fatal("update over a directory succeeded")
	}
	if got, _ := readWorking(t, co, "merge.txt"); got != "a\nb\nC\n" {
		t.Fatalf("merge.txt = %q, written by a refused update", got)
	}
	if got, _ := readWorking(t, co, "plain.txt"); got != "plain\n" {
		t.Fatalf("plain.txt = %q, written by a refused update", got)
	}
	if vid, _, _ := co.Version(); vid != a {
		t.Fatalf("version = %d, want %d", vid, a)
	}
}

// A file the update must write but cannot (read-only) is refused up front.
func TestUpdateRefusesReadOnlyFile(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "merge.txt", "a\nb\nC\n")
	if err := os.Chmod(filepath.Join(co.dir, "plain.txt"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := co.Update(UpdateOpts{TargetRID: b}); err == nil {
		t.Fatal("update over a read-only file succeeded")
	}
	if got, _ := readWorking(t, co, "merge.txt"); got != "a\nb\nC\n" {
		t.Fatalf("merge.txt = %q, written by a refused update", got)
	}
}

// A target that only makes a file executable changes its mode, as fossil's
// checkout_set_all_exe does after an update.
func TestUpdateAppliesExecBit(t *testing.T) {
	r, a, b, _ := forkRepo(t,
		[]manifest.File{{Name: "run.sh", Content: []byte("echo\n")}},
		[]manifest.File{{Name: "run.sh", Content: []byte("echo\n"), Perm: "x"}},
		nil)
	co := editsCheckout(t, r, a)
	if _, err := co.Update(UpdateOpts{TargetRID: b}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(co.dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("run.sh mode %v, want executable", info.Mode())
	}
}

// The checkout database moves to the target in one transaction: a failure
// recording the new version leaves vfile on the old one.
func TestUpdateFinalizeIsAtomic(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	if _, err := co.db.Exec(`CREATE TRIGGER fail_vvar BEFORE UPDATE ON vvar
		BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := co.db.Exec(`CREATE TRIGGER fail_vvar_ins BEFORE INSERT ON vvar
		WHEN NEW.name = 'checkout' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Update(UpdateOpts{TargetRID: b}); err == nil {
		t.Fatal("update succeeded despite the injected failure")
	}
	var other int
	if err := co.db.QueryRow("SELECT count(*) FROM vfile WHERE vid != ?", int64(a)).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if other != 0 {
		t.Fatalf("%d vfile rows left on another version", other)
	}
}

// A dry run leaves the checkout database as it was, the scan's findings
// included.
func TestUpdateDryRunLeavesDatabase(t *testing.T) {
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "clash.txt", "z\n")
	snapshot := func() string {
		rows, err := co.db.Query("SELECT id, pathname, chnged, coalesce(mtime,0) FROM vfile ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var id, chnged, mtime int64
			var name string
			if err := rows.Scan(&id, &name, &chnged, &mtime); err != nil {
				t.Fatal(err)
			}
			b.WriteString(name + ":" + strconv.FormatInt(chnged, 10) + ":" + strconv.FormatInt(mtime, 10) + " ")
		}
		return b.String()
	}
	before := snapshot()
	if _, err := co.Update(UpdateOpts{TargetRID: b, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("dry run changed vfile:\n%s\nwas\n%s", after, before)
	}
}

// A checkout root the user cannot write to stops the update before the
// first write: removing gone.txt would fail, so clash.txt must not be
// merged first.
func TestUpdateRefusesUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	r, a, b := editsRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "clash.txt", "z\n")
	if err := os.Chmod(co.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(co.dir, 0o755) })

	if _, err := co.Update(UpdateOpts{TargetRID: b}); err == nil {
		t.Fatal("update in an unwritable directory succeeded")
	}
	if got, _ := readWorking(t, co, "clash.txt"); got != "z\n" {
		t.Fatalf("clash.txt = %q, written by a refused update", got)
	}
}

// A read-only untracked file where the target adds one is backed up to
// NAME-original and replaced, as fossil does: the backup keeps its data.
func TestUpdateReplacesReadOnlyUntrackedFile(t *testing.T) {
	r, a, b, _ := forkRepo(t,
		[]manifest.File{{Name: "keep.txt", Content: []byte("keep\n")}},
		[]manifest.File{{Name: "keep.txt", Content: []byte("keep\n")}, {Name: "new.txt", Content: []byte("theirs\n")}},
		nil)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "new.txt", "mine\n")
	if err := os.Chmod(filepath.Join(co.dir, "new.txt"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := co.Update(UpdateOpts{TargetRID: b}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "new.txt"); got != "theirs\n" {
		t.Fatalf("new.txt = %q, want the target's", got)
	}
	if got, _ := readWorking(t, co, "new.txt-original"); got != "mine\n" {
		t.Fatalf("new.txt-original = %q, want the untracked file", got)
	}
}

// The binary rule holds for every strategy, not only the line merge.
func TestMergeKeepsBinaryFileForAnyStrategy(t *testing.T) {
	for _, strategy := range []string{"three-way", "last-writer-wins", "binary"} {
		r, _, left, right := forkRepo(t,
			[]manifest.File{{Name: "bin.dat", Content: []byte("a\x00b\nc\n")}},
			[]manifest.File{{Name: "bin.dat", Content: []byte("A\x00b\nc\n")}},
			[]manifest.File{{Name: "bin.dat", Content: []byte("a\x00b\nC\n")}})
		co := editsCheckout(t, r, left)
		res, err := co.Merge(MergeOpts{Version: right, Strategy: strategy})
		if err != nil {
			t.Fatalf("%s: %v", strategy, err)
		}
		if got, _ := readWorking(t, co, "bin.dat"); got != "A\x00b\nc\n" {
			t.Errorf("%s: bin.dat = %q, want the checkout's copy kept", strategy, got)
		}
		if len(res.Files) != 1 || res.Files[0].Action != MergeKept {
			t.Errorf("%s: files = %+v, want bin.dat kept", strategy, res.Files)
		}
	}
}
