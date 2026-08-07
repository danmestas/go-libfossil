package manifest

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// buildCheckin returns the marshalled manifest for a check-in and its uuid
// without storing the manifest, so a caller can store an artifact that
// references the check-in ahead of the check-in itself. The file blob is
// stored eagerly because ComputeR has to hash it either way and plain file
// blobs impose no crosslink ordering.
func buildCheckin(t *testing.T, r *repo.Repo, comment string, when time.Time, tags []deck.TagCard, parents ...string) ([]byte, string) {
	t.Helper()

	content := []byte("content for " + comment)
	fileUUID := repoHash(r, content)
	if _, _, err := blob.Store(r.DB(), content); err != nil {
		t.Fatalf("blob.Store(file for %s): %v", comment, err)
	}

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    comment,
		D:    when,
		F:    []deck.FileCard{{Name: "f.txt", UUID: fileUUID}},
		P:    parents,
		T:    tags,
		U:    deck.User("tester"),
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
	return manifestBytes, repoHash(r, manifestBytes)
}

// storeControl stores a control artifact carrying the given T-cards.
func storeControl(t *testing.T, r *repo.Repo, when time.Time, cards []deck.TagCard) string {
	t.Helper()

	d := &deck.Deck{
		Type: deck.Control,
		D:    when,
		T:    cards,
		U:    deck.User("tester"),
	}
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal(control): %v", err)
	}
	_, uuid, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("blob.Store(control): %v", err)
	}
	return uuid
}

// checkinTags returns "tagname=value/tagtype" for every tagxref row on the
// check-in with the given comment, so a failure names the tags rather than
// opaque rids.
func checkinTags(t *testing.T, r *repo.Repo, comment string) []string {
	t.Helper()
	rows, err := r.DB().Query(`
		SELECT tag.tagname, coalesce(tagxref.value, ''), tagxref.tagtype
		FROM tagxref
		JOIN tag ON tag.tagid = tagxref.tagid
		JOIN event ON event.objid = tagxref.rid AND event.type = 'ci'
		WHERE event.comment = ?`, comment)
	if err != nil {
		t.Fatalf("query tagxref for %s: %v", comment, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name, value string
		var tagtype int
		if err := rows.Scan(&name, &value, &tagtype); err != nil {
			t.Fatalf("scan tagxref: %v", err)
		}
		got = append(got, fmt.Sprintf("%s=%s/%d", name, value, tagtype))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	return got
}

func assertCheckinTags(t *testing.T, r *repo.Repo, comment string, want []string) {
	t.Helper()
	got := checkinTags(t, r, comment)
	if len(got) != len(want) {
		t.Fatalf("%s: tags = %v, want %v", comment, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: tags = %v, want %v", comment, got, want)
		}
	}
}

// TestCrosslinkTagInsertIsMtimeGuarded pins the guard canonical fossil opens
// tag_insert with (src/tag.c:173-186):
//
//	SELECT 1 FROM tagxref WHERE tagid=%d AND rid=%d AND mtime>=:mtime
//	...
//	if( rc==SQLITE_ROW ){
//	  /* Another entry that is more recent already exists.  Do nothing */
//	  return tagid;
//	}
//
// A tag application whose mtime is not strictly newer than the row already
// standing for (tagid, rid) is dropped entirely -- no tagxref write and, just
// as importantly, no tag_propagate call. That is what makes the outcome
// independent of the order artifacts are crosslinked in.
//
// Without it, a check-in's own inline T-cards clobber a *later* control
// artifact that retagged the same check-in, and then re-propagate the stale
// value to every descendant. The fixture is the shape that costs the real
// fossil repository 19,115 surplus sym-dual-license rows (issue #198):
//
//	c1 trunk ──> c2 declares branch dual-license ──> c3 ──> c4
//
//	                a later control artifact renames c2's branch:
//	                  *branch|c2|clear-title
//	                  *sym-clear-title|c2
//	                  -sym-dual-license|c2
//
// The control artifact is stored ahead of c2 so the sweep -- which visits
// candidates in ascending rid order absent delta chains -- reaches it first,
// reproducing the arrival order a clone actually produced. Canonical keeps the
// rename: c2 is on clear-title and sym-dual-license survives only as the
// cancel row, so no descendant carries it.
func TestCrosslinkTagInsertIsMtimeGuarded(t *testing.T) {
	r := setupTestRepo(t)

	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	renamedAt := base.Add(48 * time.Hour)

	c1 := storeCheckin(t, r, "c1-trunk-root", "trunk", base)

	// c2 declares branch dual-license the way `fossil commit --branch` does:
	// a propagating branch value, a propagating sym tag, and a cancel that
	// stops the parent's sym-trunk here.
	c2Bytes, c2 := buildCheckin(t, r, "c2-branch-root", base.Add(time.Minute), []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "dual-license"},
		{Type: deck.TagPropagating, Name: "sym-dual-license", UUID: "*"},
		{Type: deck.TagCancel, Name: "sym-trunk", UUID: "*"},
	}, c1)

	// Stored before c2, so it is crosslinked before c2.
	storeControl(t, r, renamedAt, []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: c2, Value: "clear-title"},
		{Type: deck.TagPropagating, Name: "sym-clear-title", UUID: c2},
		{Type: deck.TagCancel, Name: "sym-dual-license", UUID: c2},
	})

	if _, _, err := blob.Store(r.DB(), c2Bytes); err != nil {
		t.Fatalf("blob.Store(c2): %v", err)
	}
	c3 := storeCheckin(t, r, "c3-branch-mid", "", base.Add(2*time.Minute), c2)
	storeCheckin(t, r, "c4-branch-tip", "", base.Add(3*time.Minute), c3)

	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	// c2 keeps the rename: the inline cards are older than the control
	// artifact's, so none of them may overwrite it.
	assertCheckinTags(t, r, "c2-branch-root", []string{
		"branch=clear-title/2",
		"sym-clear-title=/2",
		"sym-dual-license=/0",
		"sym-trunk=/0",
	})

	// Descendants inherit the renamed branch and must not carry the
	// cancelled sym tag.
	for _, c := range []string{"c3-branch-mid", "c4-branch-tip"} {
		assertCheckinTags(t, r, c, []string{
			"branch=clear-title/2",
			"sym-clear-title=/2",
		})
	}
}

// TestCrosslinkPropagatesToLaterLinkedDescendant pins the seeding rule
// canonical fossil uses: after inserting a new check-in's plink edges it calls
// tag_propagate_all on the *primary parent* (src/manifest.c:2300-2302 and
// 2467-2469), not on the artifact that originally declared the tag.
//
// The distinction matters because tag_propagate's per-child test is strict:
//
//	coalesce(srcid=0 AND tagxref.mtime<:mtime, 1) AS doit
//
// A descendant that already carries the tag holds it at exactly the origin's
// mtime, so `mtime < mtime` is false, doit is 0, and the walk neither retags
// that child nor queues it. Replaying from the origin therefore stops dead at
// the first already-tagged descendant. Seeding from each primary parent instead
// tests only that parent's children, so a check-in linked by a later sweep --
// deferred behind a missing blob, or arriving in a later clone round -- still
// inherits its parent's branch.
//
// On the real fossil repository this was worth 693 missing propagated branch
// rows (issue #198).
func TestCrosslinkPropagatesToLaterLinkedDescendant(t *testing.T) {
	r := setupTestRepo(t)

	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c1 := storeCheckin(t, r, "c1-trunk-root", "trunk", base)
	c2 := storeCheckin(t, r, "c2-foo-root", "foo", base.Add(time.Minute), c1)
	c3 := storeCheckin(t, r, "c3-foo-mid", "", base.Add(2*time.Minute), c2)

	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink first sweep: %v", err)
	}
	// storeCheckin declares sym-foo as a singleton, so only branch propagates.
	assertCheckinTags(t, r, "c3-foo-mid", []string{"branch=foo/2"})

	// c4 arrives after the branch has already been propagated as far as c3.
	storeCheckin(t, r, "c4-foo-tip", "", base.Add(3*time.Minute), c3)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink second sweep: %v", err)
	}

	assertCheckinTags(t, r, "c4-foo-tip", []string{"branch=foo/2"})
}
