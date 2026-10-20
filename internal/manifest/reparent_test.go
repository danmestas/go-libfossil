package manifest

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// uuidOfRid is a check-in's blob hash.
func uuidOfRid(t *testing.T, q db.Querier, rid libfossil.FslID) string {
	t.Helper()
	var u string
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&u); err != nil {
		t.Fatalf("uuid of rid=%d: %v", rid, err)
	}
	return u
}

// storeParentTag stores a control artifact carrying a singleton `parent` tag
// that reparents the check-in named by child to the hashes in parents.
func storeParentTag(t *testing.T, r *repo.Repo, child string, parents []string, at time.Time) {
	t.Helper()
	d := &deck.Deck{
		Type: deck.Control,
		D:    at,
		T: []deck.TagCard{{
			Type: deck.TagSingleton, Name: "parent", UUID: child,
			Value: strings.Join(parents, " "),
		}},
		U: deck.User("testuser"),
	}
	data, err := d.Marshal()
	if err != nil {
		t.Fatalf("marshal parent tag: %v", err)
	}
	if _, _, err := blob.Store(r.DB(), data); err != nil {
		t.Fatalf("store parent tag: %v", err)
	}
}

// plinkParents returns the pids of a check-in, sorted, as uuids.
func plinkParents(t *testing.T, q db.Querier, cid libfossil.FslID) []string {
	t.Helper()
	rows, err := q.Query(`SELECT b.uuid FROM plink JOIN blob b ON b.rid=plink.pid
	                       WHERE plink.cid=?`, cid)
	if err != nil {
		t.Fatalf("plink parents of %d: %v", cid, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// leafUUIDs returns the leaf rids as uuids, sorted.
func leafUUIDs(t *testing.T, q db.Querier) []string {
	t.Helper()
	rows, err := q.Query("SELECT b.uuid FROM leaf JOIN blob b ON b.rid=leaf.rid")
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

func mlinkPrimaryParent(t *testing.T, q db.Querier, mid libfossil.FslID) int64 {
	t.Helper()
	var pmid int64
	if err := q.QueryRow(
		"SELECT pmid FROM mlink WHERE mid=? AND NOT isaux LIMIT 1", mid).Scan(&pmid); err != nil {
		t.Fatalf("primary mlink of %d: %v", mid, err)
	}
	return pmid
}

// relinkDerived clears the marks a crosslink reads as "already linked" and
// crosslinks again, as the receive/relink path does.
func relinkDerived(t *testing.T, path string) {
	t.Helper()
	r, err := repo.Open(path)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r.Close()
	for _, clear := range []string{"DELETE FROM event", "DELETE FROM tagxref WHERE srcid > 0"} {
		if _, err := r.DB().Exec(clear); err != nil {
			t.Fatalf("clear: %v", err)
		}
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("relink Crosslink: %v", err)
	}
}

// TestReparent is the issue #286 regression. A singleton parent tag moves c3
// off c2 and onto c1: its plink and mlink are rewritten. The rewrite must be
// idempotent across a second crosslink of the same artifacts.
//
// c2, left without a child, keeps the verdict it had: the parent tag is no
// branch tag and rewriting plink schedules no leaf recheck
// (draft-fossil-repo-state-00 Section 7.3), as in fossil's own crosslink of a
// parent tag. An explicit full rebuild, which is the single-batch result,
// makes it a leaf.
func TestReparent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reparent.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	commit := func(label, content string, parent libfossil.FslID, hour int) libfossil.FslID {
		t.Helper()
		rid, _, err := Checkin(r, CheckinOpts{
			Files:   []File{{Name: "f.txt", Content: []byte(content)}},
			Comment: label, User: "testuser", Parent: parent,
			Time: base.Add(time.Duration(hour) * time.Hour),
		})
		if err != nil {
			t.Fatalf("Checkin(%s): %v", label, err)
		}
		return rid
	}
	c1 := commit("c1", "v1\n", 0, 0)
	c2 := commit("c2", "v2\n", c1, 1)
	c3 := commit("c3", "v3\n", c2, 2)
	u1, u2, u3 := uuidOfRid(t, r.DB(), c1), uuidOfRid(t, r.DB(), c2), uuidOfRid(t, r.DB(), c3)

	// Before: c3's parent is c2; c3 is the only leaf; c3's mlink diffs c2.
	if got := plinkParents(t, r.DB(), c3); len(got) != 1 || got[0] != u2 {
		t.Fatalf("before: plink parents of c3 = %v, want [%s]", got, u2)
	}
	if got := leafUUIDs(t, r.DB()); len(got) != 1 || got[0] != u3 {
		t.Fatalf("before: leaves = %v, want [%s]", got, u3)
	}
	if got := mlinkPrimaryParent(t, r.DB(), c3); got != int64(c2) {
		t.Fatalf("before: c3 primary mlink pmid = %d, want %d (c2)", got, c2)
	}

	storeParentTag(t, r, u3, []string{u1}, base.Add(9*time.Hour))
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink after parent tag: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	assertAfter := func(stage string, rebuild bool, wantLeaves ...string) {
		rr, err := repo.Open(path)
		if err != nil {
			t.Fatalf("repo.Open: %v", err)
		}
		defer rr.Close()
		if rebuild {
			if err := leaf.Rebuild(rr.DB()); err != nil {
				t.Fatalf("leaf.Rebuild: %v", err)
			}
		}
		if got := plinkParents(t, rr.DB(), c3); len(got) != 1 || got[0] != u1 {
			t.Errorf("%s: plink parents of c3 = %v, want [%s] (c1)", stage, got, u1)
		}
		sort.Strings(wantLeaves)
		if got := leafUUIDs(t, rr.DB()); strings.Join(got, ",") != strings.Join(wantLeaves, ",") {
			t.Errorf("%s: leaves = %v, want %v", stage, got, wantLeaves)
		}
		if got := mlinkPrimaryParent(t, rr.DB(), c3); got != int64(c1) {
			t.Errorf("%s: c3 primary mlink pmid = %d, want %d (c1)", stage, got, c1)
		}
	}
	assertAfter("after reparent", false, u3)
	relinkDerived(t, path)
	assertAfter("after relink (idempotent)", false, u3)
	assertAfter("after leaf.Rebuild", true, u2, u3)
}

// TestReparentPhantomTarget covers a parent tag that names a parent not yet in
// the repository: the reparent reserves a phantom edge for it through the same
// ridOrPhantom path the normal plink derivation uses, so the ancestry is not
// silently dropped. (Dephantomizing on the parent's later arrival is a
// separate blob.Store concern tracked in #278, so it is not asserted here.)
func TestReparentPhantomTarget(t *testing.T) {
	// A valid-looking hash for an artifact the repository does not hold.
	const absentParent = "0123456789abcdef0123456789abcdef01234567"

	path := filepath.Join(t.TempDir(), "phantom.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer r.Close()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	c1, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "g.txt", Content: []byte("x\n")}},
		Comment: "c1", User: "testuser", Time: base,
	})
	if err != nil {
		t.Fatalf("Checkin c1: %v", err)
	}
	u1 := uuidOfRid(t, r.DB(), c1)

	// Reparent c1 onto a parent that has not arrived: the edge points at a
	// phantom reserved for it.
	storeParentTag(t, r, u1, []string{absentParent}, base.Add(9*time.Hour))
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	var phantomSize int64
	if err := r.DB().QueryRow(
		"SELECT size FROM blob WHERE uuid=?", absentParent).Scan(&phantomSize); err != nil {
		t.Fatalf("phantom lookup: %v", err)
	}
	if phantomSize >= 0 {
		t.Fatalf("absent parent blob size = %d, want < 0 (a phantom)", phantomSize)
	}
	if got := plinkParents(t, r.DB(), c1); len(got) != 1 || got[0] != absentParent {
		t.Fatalf("plink parents of c1 = %v, want [%s] (phantom)", got, absentParent)
	}
}
