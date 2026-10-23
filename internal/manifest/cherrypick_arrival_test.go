package manifest

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// A check-in that arrives before its cherrypick source must end with the
// cherrypick row and the source's auxiliary mlink rows a rebuild derives.
//
// How: the source repository holds root A, B (a child of A changing f.txt),
// and C (a child of A that cherrypicks B and also changes g.txt). The clone
// receives A and C, crosslinks, then receives B and crosslinks again, as a
// sync whose rounds deliver C first does. Its mlink and cherrypick tables are
// then compared with a rebuild of the same blobs. On fossil-scm.org's
// repository, a clone missing this had 139 cherrypick rows and 262 mlink rows
// fewer than its origin.
func TestCherrypickSourceArrivingLateMatchesRebuild(t *testing.T) {
	src := setupTestRepo(t)
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	files := map[string]string{}
	for _, name := range []string{"fa", "fb", "ga", "gc"} {
		files[name] = storeFileContent(t, src, "content "+name)
	}

	a := storeDeck(t, src, &deck.Deck{
		Type: deck.Checkin, C: "A", D: base, U: deck.User("testuser"),
		T: []deck.TagCard{{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"}},
		F: []deck.FileCard{{Name: "f.txt", UUID: files["fa"]}, {Name: "g.txt", UUID: files["ga"]}},
	})
	b := storeDeck(t, src, &deck.Deck{
		Type: deck.Checkin, C: "B", D: base.Add(time.Minute), U: deck.User("testuser"),
		P: []string{a},
		F: []deck.FileCard{{Name: "f.txt", UUID: files["fb"]}, {Name: "g.txt", UUID: files["ga"]}},
	})
	c := storeDeck(t, src, &deck.Deck{
		Type: deck.Checkin, C: "C", D: base.Add(2 * time.Minute), U: deck.User("testuser"),
		P: []string{a},
		Q: []deck.CherryPick{{Target: b}},
		F: []deck.FileCard{{Name: "f.txt", UUID: files["fb"]}, {Name: "g.txt", UUID: files["gc"]}},
	})

	dst := setupTestRepo(t)
	late := map[string]bool{b: true}
	var lateBlobs [][]byte
	for _, data := range expandAll(t, src) {
		if late[repoHash(dst, data)] {
			lateBlobs = append(lateBlobs, data)
			continue
		}
		if _, _, err := blob.Store(dst.DB(), data); err != nil {
			t.Fatalf("deliver: %v", err)
		}
	}
	if len(lateBlobs) != 1 {
		t.Fatalf("held back %d blobs, want B alone", len(lateBlobs))
	}
	if _, err := Crosslink(dst); err != nil {
		t.Fatalf("Crosslink without B: %v", err)
	}
	if _, _, err := blob.Store(dst.DB(), lateBlobs[0]); err != nil {
		t.Fatalf("deliver B: %v", err)
	}
	if _, err := Crosslink(dst); err != nil {
		t.Fatalf("Crosslink with B: %v", err)
	}

	got := linkageDigest(t, dst.DB())
	rebuildFromScratch(t, dst)
	want := linkageDigest(t, dst.DB())
	if got != want {
		t.Errorf("C=%s linked before its cherrypick source; mlink and cherrypick differ from a rebuild:\n got  %s\n want %s",
			c[:10], got, want)
	}
	var aux int
	if err := dst.DB().QueryRow("SELECT count(*) FROM mlink WHERE isaux").Scan(&aux); err != nil {
		t.Fatalf("count aux rows: %v", err)
	}
	if aux != 1 {
		t.Errorf("rebuild derived %d auxiliary mlink rows, want 1 (g.txt relative to B)", aux)
	}
}

// storeFileContent stores content in r and returns its name.
func storeFileContent(t *testing.T, r *repo.Repo, content string) string {
	t.Helper()
	if _, _, err := blob.Store(r.DB(), []byte(content)); err != nil {
		t.Fatalf("store %q: %v", content, err)
	}
	return repoHash(r, []byte(content))
}

// storeDeck marshals d, stores it in r, and returns its name.
func storeDeck(t *testing.T, r *repo.Repo, d *deck.Deck) string {
	t.Helper()
	data, err := d.Marshal()
	if err != nil {
		t.Fatalf("marshal %s: %v", d.C, err)
	}
	if _, _, err := blob.Store(r.DB(), data); err != nil {
		t.Fatalf("store %s: %v", d.C, err)
	}
	return repoHash(r, data)
}

// linkageDigest is every mlink and cherrypick row, every column, in one
// string.
func linkageDigest(t *testing.T, q db.Querier) string {
	t.Helper()
	var mlink, cherrypick string
	if err := q.QueryRow(`SELECT coalesce(group_concat(v, '|'), '') FROM
	    (SELECT mid || ':' || coalesce(fid,'') || ':' || coalesce(pmid,'') || ':' ||
	            coalesce(pid,'') || ':' || fnid || ':' || coalesce(pfnid,'') || ':' ||
	            coalesce(mperm,'') || ':' || isaux AS v
	       FROM mlink ORDER BY mid, fnid, pmid, isaux)`).Scan(&mlink); err != nil {
		t.Fatalf("digest mlink: %v", err)
	}
	if err := q.QueryRow(`SELECT coalesce(group_concat(v, '|'), '') FROM
	    (SELECT parentid || ':' || childid || ':' || isExclude AS v
	       FROM cherrypick ORDER BY parentid, childid)`).Scan(&cherrypick); err != nil {
		t.Fatalf("digest cherrypick: %v", err)
	}
	if mlink == "" {
		t.Fatal("linkageDigest: empty digest; the comparison would be vacuous")
	}
	return "mlink " + mlink + " cherrypick " + cherrypick
}
