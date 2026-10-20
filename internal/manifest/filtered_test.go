package manifest

import (
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// These tests drive the cone predicate of draft-fossil-sparse-cone-00 §4.1
// at the enumeration seam: a filtered listing is the full listing narrowed to
// the covered names, and on a tree manifest it never resolves a sub-manifest
// whose key range the cone cannot cover (§1.3).

func filteredNames(t *testing.T, r *repo.Repo, rid libfossil.FslID, f FileFilter) []string {
	t.Helper()
	files, err := ListFilesFiltered(r, rid, f)
	if err != nil {
		t.Fatalf("ListFilesFiltered: %v", err)
	}
	names := make([]string, len(files))
	for i, e := range files {
		names[i] = e.Name
	}
	return names
}

func mustCone(t *testing.T, include, exclude []string) *cone.Cone {
	t.Helper()
	c, err := cone.New(include, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListFilesFilteredFlatAndDelta(t *testing.T) {
	r := setupTestRepo(t)
	files := []File{
		{Name: "README", Content: []byte("r\n")},
		{Name: "src/a.c", Content: []byte("a\n")},
		{Name: "src/vendor/v.c", Content: []byte("v\n")},
		{Name: "src.txt", Content: []byte("s\n")},
		{Name: "tools/t.c", Content: []byte("t\n")},
	}
	parent, _ := commit(t, r, CheckinOpts{Files: files, Comment: "flat"})
	files[1].Content = []byte("a2\n")
	child, d := commit(t, r, CheckinOpts{Files: files, Comment: "delta", Parent: parent,
		Delta: true, Time: writeTime.Add(time.Hour)})
	if d.B == "" {
		t.Fatal("the child is not a delta manifest")
	}
	c := mustCone(t, []string{"src", "README"}, []string{"src/vendor"})
	for _, rid := range []libfossil.FslID{parent, child} {
		if got := strings.Join(filteredNames(t, r, rid, c), " "); got != "README src/a.c" {
			t.Errorf("rid %d: filtered = %q", rid, got)
		}
		if got := len(filteredNames(t, r, rid, nil)); got != len(files) {
			t.Errorf("rid %d: a nil filter listed %d files, want %d", rid, got, len(files))
		}
	}
}

// An out-of-cone sub-manifest the store does not hold stops the dense walk
// but not the filtered one, which never asks for it.
func TestListFilesFilteredPrunesTree(t *testing.T) {
	f := newTreeFixture(t)
	a1, a2 := f.file("a/1.c"), f.file("a/2.c")
	b1, b2 := f.file("b/1.c"), f.file("b/2.c")
	c1 := f.file("c/1.c")
	absentBytes := f.marshal(&deck.Deck{F: []deck.FileCard{b1, b2}})
	absent := deck.SubmanifestCard{Name: "b/1.c", UUID: repoHash(f.r, absentBytes)}
	// The interior node names a leaf the store lacks.
	refs := []deck.SubmanifestCard{f.leaf(a1, a2), f.interior(absent, f.leaf(c1))}
	rid, _ := f.checkin(treeDeck(treeTime, refs))

	if _, err := ListFiles(f.r, rid); err == nil {
		t.Fatal("the dense walk listed a tree with a missing sub-manifest")
	}
	for _, tc := range []struct {
		include, exclude []string
		want             string
	}{
		{[]string{"a"}, nil, "a/1.c a/2.c"},
		// "c" could own a "c/0.c" in the absent leaf, which ends at
		// "c/1.c"; the file member alone rules that leaf out.
		{[]string{"c/1.c"}, nil, "c/1.c"},
		{[]string{"a/2.c", "c/1.c"}, nil, "a/2.c c/1.c"},
		{[]string{"a"}, []string{"a/1.c"}, "a/2.c"},
		{[]string{"zzz"}, nil, ""},
	} {
		got := strings.Join(filteredNames(t, f.r, rid, mustCone(t, tc.include, tc.exclude)), " ")
		if got != tc.want {
			t.Errorf("cone %q !%q: filtered = %q, want %q", tc.include, tc.exclude, got, tc.want)
		}
	}
	// A cone that reaches into the absent node still fails, as it must.
	if _, err := ListFilesFiltered(f.r, rid, mustCone(t, []string{"b"}, nil)); err == nil {
		t.Fatal("a cone over the missing sub-manifest listed without it")
	} else if got, ok := MissingSubmanifest(err); !ok || got != absent.UUID {
		t.Fatalf("error %v, want the missing %s", err, absent.UUID)
	}
}

// A carried file names an artifact already in the repository by its hash,
// with no content: the F card keeps that name verbatim and the R card still
// covers it (sparse-cone §4.2, §4.3).
func TestCheckinCarriesFileByHash(t *testing.T) {
	r := setupTestRepo(t)
	parent, pd := commit(t, r, CheckinOpts{Files: []File{
		{Name: "in.c", Content: []byte("in\n")},
		{Name: "out.c", Content: []byte("out\n")},
	}, Comment: "one"})
	outUUID := pd.F[1].UUID
	child, cd := commit(t, r, CheckinOpts{Files: []File{
		{Name: "in.c", Content: []byte("in 2\n")},
		{Name: "out.c", UUID: outUUID, Perm: "x"},
	}, Comment: "two", Parent: parent, Time: writeTime.Add(time.Hour)})
	if cd.F[1].Name != "out.c" || cd.F[1].UUID != outUUID || cd.F[1].Perm != "x" {
		t.Fatalf("carried F card = %+v", cd.F[1])
	}
	if err := r.WithTx(func(tx *db.Tx) error { return VerifyRCard(tx, nil, child) }); err != nil {
		t.Fatalf("R card: %v", err)
	}

	missing := strings.Repeat("ab", 20)
	_, _, err := Checkin(r, CheckinOpts{Files: []File{{Name: "gone.c", UUID: missing}},
		Comment: "three", User: "u", Parent: child, Time: writeTime.Add(2 * time.Hour)})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("carrying an absent artifact: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a carried file with content did not panic")
		}
	}()
	_, _, _ = Checkin(r, CheckinOpts{Files: []File{{Name: "x", UUID: outUUID, Content: []byte("x")}},
		Comment: "four", User: "u", Parent: child, Time: writeTime.Add(3 * time.Hour)})
}
