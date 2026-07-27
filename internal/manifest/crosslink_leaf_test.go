package manifest

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// storeCheckin builds a check-in manifest with the given parents and branch
// declaration and stores it as a blob without crosslinking it, mirroring the
// receiver state after a sync round delivers manifests but before the sweep
// runs. branch == "" declares no branch tag, so the check-in inherits its
// primary parent's branch through tag propagation the way a plain commit does.
func storeCheckin(t *testing.T, r *repo.Repo, comment, branch string, when time.Time, parents ...string) string {
	t.Helper()

	content := []byte("content for " + comment)
	fileUUID := hash.SHA1(content)
	if _, _, err := blob.Store(r.DB(), content); err != nil {
		t.Fatalf("blob.Store(file for %s): %v", comment, err)
	}

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    comment,
		D:    when,
		F:    []deck.FileCard{{Name: "f.txt", UUID: fileUUID}},
		P:    parents,
		U:    deck.User("tester"),
	}
	if branch != "" {
		d.T = []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: branch},
			{Type: deck.TagSingleton, Name: "sym-" + branch, UUID: "*"},
		}
	}
	rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
		if uuid == fileUUID {
			return content, nil
		}
		return nil, fmt.Errorf("unknown uuid: %s", uuid)
	})
	if err != nil {
		t.Fatalf("ComputeR(%s): %v", comment, err)
	}
	d.R = rHash

	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal(%s): %v", comment, err)
	}
	_, uuid, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("blob.Store(manifest %s): %v", comment, err)
	}
	return uuid
}

// leafComments returns the comment of every check-in in the leaf table, so an
// assertion failure names the check-ins rather than opaque rids.
func leafComments(t *testing.T, r *repo.Repo) []string {
	t.Helper()
	rows, err := r.DB().Query(`
		SELECT e.comment FROM leaf l JOIN event e ON e.objid = l.rid
		WHERE e.type = 'ci'`)
	if err != nil {
		t.Fatalf("query leaf: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan leaf: %v", err)
		}
		got = append(got, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	return got
}

func assertLeaves(t *testing.T, r *repo.Repo, where string, want []string) {
	t.Helper()
	got := leafComments(t, r)
	if len(got) != len(want) {
		t.Fatalf("%s: leaf = %v, want %v", where, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: leaf = %v, want %v", where, got, want)
		}
	}
}

// TestCrosslinkLeafMatchesCanonicalBranchRule pins the leaf rule canonical
// fossil implements in src/leaf.c: a check-in is a leaf when no plink child of
// it carries the *same* branch name, where a check-in with no branch tagxref
// value counts as "trunk".
//
//	leaf_check(): SELECT 1 FROM plink WHERE pid=:rid
//	    AND coalesce((SELECT value FROM tagxref WHERE tagid=TAG_BRANCH
//	                   AND rid=:rid),'trunk')
//	     == coalesce((SELECT value FROM tagxref WHERE tagid=TAG_BRANCH
//	                   AND rid=plink.cid),'trunk')
//
// Neither "has any child" nor "has a primary child" is the rule, and each
// disagrees with fossil on one arm of the fixture below (issue #189):
//
//		c1 trunk ─┬─> c3 trunk ─┐
//		          │             ├─> c4 trunk(merge) ──> c5 bar ──> c6 (inherits bar)
//		          └─> c2 foo ───┘
//
//	  - c2 is the tip of branch foo, merged into trunk at c4. It has a child, so
//	    an any-child rule drops it; fossil keeps it because c4 is on trunk. This
//	    is the class that lost 939 of 1,426 leaves on the real corpus.
//	  - c4's only child c5 is its *primary* child, so an isprim rule drops it;
//	    fossil keeps it because c5 is on bar.
//	  - c5 is not a leaf even though c6 declares no branch of its own: c6
//	    inherits bar by tag propagation, which is why the leaf recompute has to
//	    run after propagation rather than before it.
//
// The fixture is built from stored manifests and linked by Crosslink, so it
// runs in the default suite with no fossil binary.
func TestCrosslinkLeafMatchesCanonicalBranchRule(t *testing.T) {
	r := setupTestRepo(t)

	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c1 := storeCheckin(t, r, "c1-trunk-root", "trunk", base)
	c2 := storeCheckin(t, r, "c2-foo-tip", "foo", base.Add(time.Minute), c1)
	c3 := storeCheckin(t, r, "c3-trunk", "", base.Add(2*time.Minute), c1)
	c4 := storeCheckin(t, r, "c4-trunk-merge", "", base.Add(3*time.Minute), c3, c2)
	c5 := storeCheckin(t, r, "c5-bar-root", "bar", base.Add(4*time.Minute), c4)
	storeCheckin(t, r, "c6-bar-tip", "", base.Add(5*time.Minute), c5)

	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked != 6 {
		t.Fatalf("Crosslink linked %d checkins, want 6", linked)
	}

	assertLeaves(t, r, "after Crosslink", []string{
		"c2-foo-tip",     // merged branch tip: child c4 is on trunk
		"c4-trunk-merge", // branch point: primary child c5 is on bar
		"c6-bar-tip",     // ordinary tip
	})
}

// TestCheckinLeafMatchesCanonicalBranchRule is the same rule on the local
// commit path. manifest.Checkin maintained leaf by deleting every parent of the
// new check-in, which is the any-child rule, so committing onto a new branch
// silently retired the branch point (issue #189).
func TestCheckinLeafMatchesCanonicalBranchRule(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	trunkTip, _, err := Checkin(r, CheckinOpts{
		Comment: "c1-trunk-root",
		User:    "tester",
		Time:    base,
		Files:   []File{{Name: "f.txt", Content: []byte("one")}},
	})
	if err != nil {
		t.Fatalf("Checkin c1: %v", err)
	}
	assertLeaves(t, r, "after root checkin", []string{"c1-trunk-root"})

	if _, _, err := Checkin(r, CheckinOpts{
		Comment: "c2-foo-root",
		User:    "tester",
		Time:    base.Add(time.Minute),
		Parent:  trunkTip,
		Files:   []File{{Name: "f.txt", Content: []byte("two")}},
		Tags: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "foo"},
			{Type: deck.TagSingleton, Name: "sym-foo", UUID: "*"},
		},
	}); err != nil {
		t.Fatalf("Checkin c2: %v", err)
	}

	// The branch point keeps its leaf row: its only child is on branch foo.
	assertLeaves(t, r, "after branch checkin", []string{"c1-trunk-root", "c2-foo-root"})

	if _, _, err := Checkin(r, CheckinOpts{
		Comment: "c3-trunk",
		User:    "tester",
		Time:    base.Add(2 * time.Minute),
		Parent:  trunkTip,
		Files:   []File{{Name: "f.txt", Content: []byte("three")}},
	}); err != nil {
		t.Fatalf("Checkin c3: %v", err)
	}

	// c3 is on trunk, so now the branch point does lose its leaf row.
	assertLeaves(t, r, "after trunk checkin", []string{"c2-foo-root", "c3-trunk"})
}

// TestRepairLeafTableKeepsParentlessCheckin pins the one place our recompute
// deliberately differs from fossil's leaf_rebuild(), whose candidate set is
// `SELECT cid FROM plink` and so cannot return a check-in that has no parent.
// Fossil reaches that check-in through leaf_check() instead -- a fresh
// repository holding only its initial empty check-in has one leaf row -- and
// selecting candidates from event reproduces that without a second pass.
func TestRepairLeafTableKeepsParentlessCheckin(t *testing.T) {
	r := setupTestRepo(t)
	storeCheckin(t, r, "c1-only", "trunk", time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC))

	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	assertLeaves(t, r, "single parentless checkin", []string{"c1-only"})

	var plinks int
	if err := r.DB().QueryRow("SELECT count(*) FROM plink").Scan(&plinks); err != nil {
		t.Fatalf("count plink: %v", err)
	}
	if plinks != 0 {
		t.Fatalf("plink rows = %d, want 0 (fixture must have no parent edges)", plinks)
	}
}
