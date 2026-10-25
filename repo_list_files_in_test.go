package libfossil

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	fsltype "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// These tests drive Repo.ListFilesIn, the public listing narrowed to a cone
// (draft-fossil-sparse-cone-00 §4.1, §4.5): it is ListFiles filtered by
// Cone.Covers, and on a tree manifest it never reads a node the cone cannot
// reach.

// newListConeRepo commits sixteen files under a, b, c and d, as a tree when
// tree is set, and returns the repository and the check-in's rid.
func newListConeRepo(t *testing.T, tree bool) (*Repo, int64) {
	t.Helper()
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if tree {
		if _, _, err := r.EnableSubmanifests(1); err != nil {
			t.Fatal(err)
		}
	}
	var files []FileToCommit
	for _, dir := range []string{"a", "b", "c", "d"} {
		for i := 0; i < 4; i++ {
			name := fmt.Sprintf("%s/%d.c", dir, i)
			files = append(files, FileToCommit{Name: name, Content: []byte(name + "\n")})
		}
	}
	rid, _, err := r.Commit(CommitOpts{Files: files, Comment: "c", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := manifest.GetManifest(r.Inner(), fsltype.FslID(rid))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(d.S) > 0; got != tree {
		t.Fatalf("check-in is a tree: %v, want %v", got, tree)
	}
	return r, rid
}

func mustNewCone(t *testing.T, include, exclude []string) *Cone {
	t.Helper()
	c, err := NewCone(include, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func entryNames(files []FileEntry) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return strings.Join(names, " ")
}

// On flat and tree check-ins alike the narrowed listing is the full listing
// filtered by Covers, entry for entry, and a nil cone lists everything.
func TestListFilesInMatchesFilteredListFiles(t *testing.T) {
	for _, tree := range []bool{false, true} {
		r, rid := newListConeRepo(t, tree)
		all, err := r.ListFiles(rid)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.ListFilesIn(rid, nil)
		if err != nil || !reflect.DeepEqual(got, all) {
			t.Fatalf("tree %v: nil cone = %v, %v; want ListFiles", tree, got, err)
		}
		for _, c := range []*Cone{
			mustNewCone(t, []string{"b"}, nil),
			mustNewCone(t, []string{"a", "c/2.c"}, []string{"a/1.c"}),
			mustNewCone(t, []string{"zzz"}, nil),
		} {
			var want []FileEntry
			for _, f := range all {
				if c.Covers(f.Name) {
					want = append(want, f)
				}
			}
			got, err := r.ListFilesIn(rid, c)
			if err != nil {
				t.Fatalf("tree %v, cone %v: %v", tree, c, err)
			}
			if len(got) != len(want) || len(want) > 0 && !reflect.DeepEqual(got, want) {
				t.Errorf("tree %v, cone %v: %q, want %q", tree, c, entryNames(got), entryNames(want))
			}
		}
	}
}

// elideOutside makes every top-level node of the tree check-in rid whose key
// range c cannot reach an elided phantom, as a sparse store holds it, and
// returns how many it elided.
func elideOutside(t *testing.T, r *Repo, rid int64, c *Cone) int {
	t.Helper()
	d, err := manifest.GetManifest(r.Inner(), fsltype.FslID(rid))
	if err != nil {
		t.Fatal(err)
	}
	elided := 0
	for i, s := range d.S {
		limit := ""
		if i+1 < len(d.S) {
			limit = d.S[i+1].Name
		}
		if c.inner.MayCover(s.Name, limit) {
			continue
		}
		for _, stmt := range []string{
			"UPDATE blob SET size=-1,content=NULL WHERE uuid=?",
			"INSERT OR IGNORE INTO phantom SELECT rid FROM blob WHERE uuid=?",
		} {
			if _, err := r.Inner().DB().Exec(stmt, s.UUID); err != nil {
				t.Fatal(err)
			}
		}
		elided++
	}
	return elided
}

// Over a sparse store the narrowed listing reads only the nodes the cone
// reaches, so it succeeds where ListFiles fails on an elided node, and a cone
// that does reach one fails naming it.
func TestListFilesInSkipsElidedNodes(t *testing.T) {
	r, rid := newListConeRepo(t, true)
	c := mustNewCone(t, []string{"a"}, nil)
	if n := elideOutside(t, r, rid, c); n == 0 {
		t.Fatal("no node lies outside the cone")
	}
	if _, err := r.ListFiles(rid); err == nil {
		t.Fatal("ListFiles walked a tree with elided nodes")
	}
	got, err := r.ListFilesIn(rid, c)
	if err != nil {
		t.Fatal(err)
	}
	if names := entryNames(got); names != "a/0.c a/1.c a/2.c a/3.c" {
		t.Fatalf("ListFilesIn = %q", names)
	}
	if _, err := r.ListFilesIn(rid, mustNewCone(t, []string{"d"}, nil)); err == nil {
		t.Fatal("a cone over an elided node listed without it")
	} else if _, ok := manifest.MissingSubmanifest(err); !ok {
		t.Fatalf("error %v does not name the missing node", err)
	}
}

// The narrowed listing reads through the attached content cache: a narrow
// cone expands fewer artifacts than the full listing, and repeating it
// expands none.
func TestListFilesInUsesContentCache(t *testing.T) {
	r, rid := newListConeRepo(t, true)
	r.SetContentCache(1 << 20)
	c := mustNewCone(t, []string{"a"}, nil)
	if _, err := r.ListFilesIn(rid, c); err != nil {
		t.Fatal(err)
	}
	narrow := cacheStats(t, r).Misses
	if _, err := r.ListFilesIn(rid, c); err != nil {
		t.Fatal(err)
	}
	if again := cacheStats(t, r).Misses - narrow; narrow == 0 || again != 0 {
		t.Fatalf("misses: first %d, repeat %d; want some, then none", narrow, again)
	}
	if _, err := r.ListFiles(rid); err != nil {
		t.Fatal(err)
	}
	if full := cacheStats(t, r).Misses; full <= narrow {
		t.Fatalf("misses after the full listing %d, after the narrow one %d", full, narrow)
	}
}
