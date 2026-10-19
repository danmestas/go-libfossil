package manifest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// countPropagation replaces the repair gate's propagation step with one that
// records every parent it is asked to propagate from, for the test's lifetime.
func countPropagation(t *testing.T) *[]libfossil.FslID {
	t.Helper()
	var visited []libfossil.FslID
	orig := propagateAllLeaves
	propagateAllLeaves = func(q db.Querier, rid libfossil.FslID, leaves *leaf.Pending) error {
		visited = append(visited, rid)
		return orig(q, rid, leaves)
	}
	t.Cleanup(func() { propagateAllLeaves = orig })
	return &visited
}

func ridOf(t *testing.T, r *repo.Repo, uuid string) libfossil.FslID {
	t.Helper()
	var rid int64
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		t.Fatalf("rid of %s: %v", uuid, err)
	}
	return libfossil.FslID(rid)
}

// tagLeafState digests the tagxref and leaf tables, every column propagation
// and the leaf rule write, so two repositories compare row for row.
func tagLeafState(t *testing.T, r *repo.Repo) (tags, leaves string) {
	t.Helper()
	if err := r.DB().QueryRow(`SELECT coalesce(group_concat(v, '|'), '') FROM
	    (SELECT rid || ':' || tagid || ':' || tagtype || ':' || coalesce(srcid,'') || ':' ||
	            coalesce(origid,'') || ':' || coalesce(value,'') || ':' || mtime AS v
	       FROM tagxref ORDER BY rid, tagid)`).Scan(&tags); err != nil {
		t.Fatalf("digest tagxref: %v", err)
	}
	if err := r.DB().QueryRow(`SELECT coalesce(group_concat(rid, '|'), '') FROM
	    (SELECT rid FROM leaf ORDER BY rid)`).Scan(&leaves); err != nil {
		t.Fatalf("digest leaf: %v", err)
	}
	if tags == "" || leaves == "" {
		t.Fatal("tagLeafState: empty digest; the comparison would be vacuous")
	}
	return tags, leaves
}

// assertMatchesFullRebuild compares r's tag and leaf tables with what a
// rebuild from scratch derives from the same blobs, the single-batch result
// every incremental order must converge to. It leaves r rebuilt.
func assertMatchesFullRebuild(t *testing.T, r *repo.Repo) {
	t.Helper()
	gotTags, gotLeaves := tagLeafState(t, r)
	wantTags, wantLeaves := rebuildFromScratch(t, r)
	if gotTags != wantTags {
		t.Errorf("tagxref differs from a full rebuild:\n got  %s\n want %s", gotTags, wantTags)
	}
	if gotLeaves != wantLeaves {
		t.Errorf("leaf differs from a full rebuild:\n got  %s\n want %s", gotLeaves, wantLeaves)
	}
}

// rebuildFromScratch empties the derived tables, crosslinks every artifact in
// one batch, and returns the resulting tag and leaf digests.
func rebuildFromScratch(t *testing.T, r *repo.Repo) (tags, leaves string) {
	t.Helper()
	err := r.WithTx(func(tx *db.Tx) error {
		for _, table := range derivedTables {
			var n int
			if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?",
				table).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			if _, err := tx.Exec("DELETE FROM " + table); err != nil {
				return err
			}
		}
		_, err := CrosslinkRebuild(tx)
		return err
	})
	if err != nil {
		t.Fatalf("full rebuild: %v", err)
	}
	return tagLeafState(t, r)
}

// The repair gate re-propagates only from the parents its batch gave a child,
// not from every primary parent in the repository (issue #432): a sync that
// links one check-in onto a long history visits one parent.
func TestRepairVisitsOnlyBatchParents(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	tip := storeCheckin(t, r, "c1", "trunk", base)
	for i := 2; i <= 6; i++ {
		tip = storeCheckin(t, r, "c"+string(rune('0'+i)), "", base.Add(time.Duration(i)*time.Minute), tip)
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink history: %v", err)
	}

	visited := countPropagation(t)
	storeCheckin(t, r, "c7", "", base.Add(7*time.Minute), tip)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c7: %v", err)
	}
	if want := []libfossil.FslID{ridOf(t, r, tip)}; !slices.Equal(*visited, want) {
		t.Fatalf("gate propagated from %v, want only c7's parent %v", *visited, want)
	}
	assertCheckinTags(t, r, "c7", []string{"branch=trunk/2"})
	assertMatchesFullRebuild(t, r)
}

// A parent linked after its child converges: the child's batch records the
// parent while it is still a phantom, and the parent's batch records the
// grandparent, whose propagation cascades through the parent to the child.
func TestRepairConvergesParentLinkedAfterChild(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	c1 := storeCheckin(t, r, "c1", "trunk", base)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c1: %v", err)
	}

	c2Bytes, c2 := buildCheckin(t, r, "c2", base.Add(time.Minute), []deck.TagCard{
		{Type: deck.TagPropagating, Name: "sym-feature", UUID: "*"},
	}, c1)
	c3 := storeCheckin(t, r, "c3", "", base.Add(2*time.Minute), c2)
	storeCheckin(t, r, "c4", "", base.Add(3*time.Minute), c3)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c3, c4 before c2: %v", err)
	}
	// c2 is a phantom: nothing reaches its descendants yet.
	assertCheckinTags(t, r, "c4", nil)

	visited := countPropagation(t)
	if _, _, err := blob.Store(r.DB(), c2Bytes); err != nil {
		t.Fatalf("blob.Store(c2): %v", err)
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c2: %v", err)
	}
	if want := []libfossil.FslID{ridOf(t, r, c1)}; !slices.Equal(*visited, want) {
		t.Fatalf("gate propagated from %v, want only c2's parent %v", *visited, want)
	}
	for _, c := range []string{"c3", "c4"} {
		assertCheckinTags(t, r, c, []string{"branch=trunk/2", "sym-feature=/2"})
	}
	assertMatchesFullRebuild(t, r)
}

// A reparent's new parent is propagated from: the batch records the parents
// the rewritten plink rows name, so c3 moves to c2's branch.
func TestRepairConvergesAfterReparent(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	c1 := storeCheckin(t, r, "c1", "trunk", base)
	c2 := storeCheckin(t, r, "c2", "alpha", base.Add(time.Minute), c1)
	c3 := storeCheckin(t, r, "c3", "", base.Add(2*time.Minute), c1)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink history: %v", err)
	}
	assertCheckinTags(t, r, "c3", []string{"branch=trunk/2"})

	visited := countPropagation(t)
	storeParentTag(t, r, c3, []string{c2}, base.Add(time.Hour))
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink parent tag: %v", err)
	}
	if want := []libfossil.FslID{ridOf(t, r, c2)}; !slices.Equal(*visited, want) {
		t.Fatalf("gate propagated from %v, want only the new parent %v", *visited, want)
	}
	assertCheckinTags(t, r, "c3", []string{"branch=alpha/2", "parent=" + c2 + "/1"})
	// c1, left without a trunk child, keeps the verdict it had: rewriting
	// plink schedules no recheck (see TestReparent), so only the tags are the
	// single-batch result here.
	gotTags, _ := tagLeafState(t, r)
	assertLeaves(t, r, "after reparent", []string{"c3"})
	if wantTags, _ := rebuildFromScratch(t, r); gotTags != wantTags {
		t.Errorf("tagxref differs from a full rebuild:\n got  %s\n want %s", gotTags, wantTags)
	}
}

// A phantom-fill cascade's links converge: the check-ins a cascade links
// inherit their parents' tags by the time the session's gate has run.
func TestRepairConvergesAfterCascade(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	c1 := storeCheckin(t, r, "c1", "trunk", base)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c1: %v", err)
	}
	visited := countPropagation(t)
	c2 := storeCheckin(t, r, "c2", "", base.Add(time.Minute), c1)
	cascaded := AfterDephantomize(context.Background(), r, ridOf(t, r, c2))
	if cascaded != 1 {
		t.Fatalf("cascade linked %d artifacts, want 1", cascaded)
	}
	if _, err := CrosslinkAfterCascades(context.Background(), r, cascaded); err != nil {
		t.Fatalf("CrosslinkAfterCascades: %v", err)
	}
	if want := []libfossil.FslID{ridOf(t, r, c1)}; !slices.Equal(*visited, want) {
		t.Fatalf("propagated from %v, want only c2's parent %v", *visited, want)
	}
	assertCheckinTags(t, r, "c2", []string{"branch=trunk/2"})
	assertMatchesFullRebuild(t, r)
}

// The parents a transaction records roll back with it, alongside the leaf
// rechecks, so a retry neither loses one nor keeps one for rows that are gone.
func TestReceiveLinkerWithTxDropsParentsOnFailure(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	c1 := storeCheckin(t, r, "c1", "trunk", base)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c1: %v", err)
	}
	body, c2 := buildCheckin(t, r, "c2", base.Add(time.Minute), nil, c1)
	rid, _, err := blob.Store(r.DB(), body)
	if err != nil {
		t.Fatalf("blob.Store(c2): %v", err)
	}
	linker, err := NewReceiveLinker(r)
	if err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}
	boom := errors.New("later step failed")
	err = linker.WithTx(r, func(tx *db.Tx) error {
		if err := linker.LinkStored(context.Background(), tx, rid, c2, body); err != nil {
			return err
		}
		if got := linker.state.sched.parents.order; len(got) != 1 {
			t.Fatalf("LinkStored recorded parents %v, want c2's one", got)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx error = %v, want %v", err, boom)
	}
	if got := linker.state.sched.parents.order; len(got) != 0 {
		t.Fatalf("parents after a rolled-back transaction = %v, want none", got)
	}
}

func TestBatchScheduleRollbackKeepsEarlierParents(t *testing.T) {
	s := newBatchSchedule()
	s.parents.add(3)
	s.parents.add(1)
	m := s.mark()
	s.parents.add(2)
	s.parents.add(1) // already recorded before the mark: survives the rollback
	s.rollback(m)
	if got := s.parents.sorted(); !slices.Equal(got, []libfossil.FslID{1, 3}) {
		t.Fatalf("parents after rollback = %v, want [1 3]", got)
	}
	s.reset()
	if got := s.parents.sorted(); len(got) != 0 {
		t.Fatalf("parents after reset = %v, want none", got)
	}
}
