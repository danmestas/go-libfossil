package checkout

import (
	"os"
	"path/filepath"
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
