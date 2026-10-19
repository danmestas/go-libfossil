package checkout

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// renamesRepo commits A and two children of it:
//
//	A: f.txt "a\nb\nc\n", g.txt "x\ny\nz\n", h.txt "1\n2\n3\n"
//	B: f.txt renamed f2.txt and its first line edited ("A"); g.txt renamed
//	   g2.txt unchanged; h.txt unchanged
//	C: h.txt renamed h2.txt and its first line edited ("one"); f, g as in A
func renamesRepo(t *testing.T) (r *repo.Repo, a, b, c libfossil.FslID) {
	t.Helper()
	r, err := repo.CreateWithEnv(filepath.Join(t.TempDir(), "rn.fossil"), "test", simio.RealEnv(), "")
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
	f := manifest.File{Name: "f.txt", Content: []byte("a\nb\nc\n")}
	g := manifest.File{Name: "g.txt", Content: []byte("x\ny\nz\n")}
	h := manifest.File{Name: "h.txt", Content: []byte("1\n2\n3\n")}
	a = checkin(0, 1, []manifest.File{f, g, h})
	b = checkin(a, 2, []manifest.File{
		{Name: "f2.txt", Content: []byte("A\nb\nc\n"), OldName: "f.txt"},
		{Name: "g2.txt", Content: g.Content, OldName: "g.txt"},
		h,
	})
	c = checkin(a, 3, []manifest.File{
		f, g, {Name: "h2.txt", Content: []byte("one\n2\n3\n"), OldName: "h.txt"},
	})
	return r, a, b, c
}

// A local edit to a file the target renamed and changed is merged into the
// renamed file, and the old name goes, as fossil's update does.
func TestUpdateMergesEditIntoRenamedFile(t *testing.T) {
	r, a, b, _ := renamesRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "f.txt", "a\nb\nC\n")

	res, err := co.Update(UpdateOpts{TargetRID: b})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "f2.txt"); got != "A\nb\nC\n" {
		t.Fatalf("f2.txt = %q, want both edits", got)
	}
	if _, ok := readWorking(t, co, "f.txt"); ok {
		t.Fatal("f.txt still on disk")
	}
	if !containsPath(res.FilesWritten, "f2.txt") || !containsPath(res.FilesRemoved, "f.txt") {
		t.Fatalf("result = %+v, want f2.txt written and f.txt removed", res)
	}
	if got := statusOf(t, co); len(got) != 1 || got["f2.txt"] != ChangeModified {
		t.Fatalf("status = %v, want f2.txt modified", got)
	}
}

// A conflicting local edit to a renamed file gives markers in the new name.
func TestUpdateConflictInRenamedFile(t *testing.T) {
	r, a, b, _ := renamesRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "f.txt", "mine\nb\nc\n")

	res, err := co.Update(UpdateOpts{TargetRID: b})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "f2.txt"); !strings.Contains(got, "<<<<<<<") {
		t.Fatalf("f2.txt = %q, want conflict markers", got)
	}
	if strings.Join(res.Conflicted, ",") != "f2.txt" {
		t.Fatalf("Conflicted = %v, want [f2.txt]", res.Conflicted)
	}
}

// When the target only renamed the file, fossil's update leaves the edited
// file where it is, untracked, and does not write the new name: there is
// nothing to merge (the artifact is the same). Unmerged reports it.
func TestUpdateRenameOnlyKeepsEditLikeFossil(t *testing.T) {
	r, a, b, _ := renamesRepo(t)
	co := editsCheckout(t, r, a)
	writeWorking(t, co, "g.txt", "x\ny\nZ\n")

	res, err := co.Update(UpdateOpts{TargetRID: b})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "g.txt"); got != "x\ny\nZ\n" {
		t.Fatalf("g.txt = %q, want the local edit kept", got)
	}
	if _, ok := readWorking(t, co, "g2.txt"); ok {
		t.Fatal("g2.txt written; fossil leaves it missing")
	}
	if strings.Join(res.Unmerged, ",") != "g.txt" {
		t.Fatalf("Unmerged = %v, want [g.txt]", res.Unmerged)
	}
	if got := statusOf(t, co); got["g2.txt"] != ChangeMissing {
		t.Fatalf("status = %v, want g2.txt missing", got)
	}
}

// Renames on the current version's side are followed backwards: from C
// (h.txt renamed h2.txt) to its sibling B, a local edit to h2.txt is merged
// into h.txt.
func TestUpdateFollowsRenameBackToAncestor(t *testing.T) {
	r, _, b, c := renamesRepo(t)
	co := editsCheckout(t, r, c)
	writeWorking(t, co, "h2.txt", "one\n2\nthree\n")

	if _, err := co.Update(UpdateOpts{TargetRID: b}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readWorking(t, co, "h.txt"); got != "1\n2\nthree\n" {
		t.Fatalf("h.txt = %q, want the local edit on B's content", got)
	}
	if _, ok := readWorking(t, co, "h2.txt"); ok {
		t.Fatal("h2.txt still on disk")
	}
}
