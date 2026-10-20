package manifest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
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
	tags = tagDigest(t, r.DB())
	if err := r.DB().QueryRow(`SELECT coalesce(group_concat(rid, '|'), '') FROM
	    (SELECT rid FROM leaf ORDER BY rid)`).Scan(&leaves); err != nil {
		t.Fatalf("digest leaf: %v", err)
	}
	if tags == "" || leaves == "" {
		t.Fatal("tagLeafState: empty digest; the comparison would be vacuous")
	}
	return tags, leaves
}

// tagDigest is every tagxref row, every column, in one string.
func tagDigest(t *testing.T, q db.Querier) string {
	t.Helper()
	var tags string
	if err := q.QueryRow(`SELECT coalesce(group_concat(v, '|'), '') FROM
	    (SELECT rid || ':' || tagid || ':' || tagtype || ':' || coalesce(srcid,'') || ':' ||
	            coalesce(origid,'') || ':' || coalesce(value,'') || ':' || mtime AS v
	       FROM tagxref ORDER BY rid, tagid)`).Scan(&tags); err != nil {
		t.Fatalf("digest tagxref: %v", err)
	}
	return tags
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
// the rewritten plink rows name, so c3 moves to c2's branch. Here both parents
// carry only the branch tag, and the new parent's is the newer, so the result
// matches a rebuild. That does not hold in general: a propagating tag only the
// old parent carried is never removed (see ReparentCheckins).
func TestRepairPropagatesFromReparentedParent(t *testing.T) {
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
	assertLeaves(t, r, "after reparent", []string{"c3"})

	// The rewrite is settled: a later gate neither redoes it nor propagates
	// from c2 again.
	*visited = nil
	storeCheckin(t, r, "c4", "", base.Add(3*time.Minute), c3)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c4: %v", err)
	}
	if want := []libfossil.FslID{ridOf(t, r, c3)}; !slices.Equal(*visited, want) {
		t.Fatalf("later gate propagated from %v, want only c4's parent %v", *visited, want)
	}
	assertCheckinTags(t, r, "c4", []string{"branch=alpha/2"})
	gotTags, _ := tagLeafState(t, r)
	if wantTags, _ := rebuildFromScratch(t, r); gotTags != wantTags {
		t.Errorf("tagxref differs from a full rebuild:\n got  %s\n want %s", gotTags, wantTags)
	}
}

// A phantom-fill cascade's batch propagates from the parents it linked, and
// only those, before it commits; the session's gate after it owes nothing.
func TestRepairConvergesAfterCascade(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	c1 := storeCheckin(t, r, "c1", "trunk", base)
	o1 := storeCheckin(t, r, "o1", "other", base.Add(-time.Hour))
	storeCheckin(t, r, "o2", "", base.Add(-time.Hour+time.Minute), o1)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c1: %v", err)
	}
	visited := countPropagation(t)
	c2 := storeCheckin(t, r, "c2", "", base.Add(time.Minute), c1)
	cascaded := AfterDephantomize(context.Background(), r, ridOf(t, r, c2))
	if cascaded != 1 {
		t.Fatalf("cascade linked %d artifacts, want 1", cascaded)
	}
	if want := []libfossil.FslID{ridOf(t, r, c1)}; !slices.Equal(*visited, want) {
		t.Fatalf("cascade propagated from %v, want only c2's parent %v", *visited, want)
	}
	assertCheckinTags(t, r, "c2", []string{"branch=trunk/2"})
	if _, err := CrosslinkContext(context.Background(), r); err != nil {
		t.Fatalf("CrosslinkContext: %v", err)
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

// errRolledBack ends a probe transaction so it leaves nothing behind.
var errRolledBack = errors.New("probe rolled back")

// assertFullyPropagated fails if propagating from every primary parent in the
// repository would still write a tagxref row: a committed plink edge whose
// parent's tags never reached the child. The probe runs in a transaction it
// rolls back.
func assertFullyPropagated(t *testing.T, r *repo.Repo, where string) {
	t.Helper()
	before := tagDigest(t, r.DB())
	var after string
	err := r.WithTx(func(tx *db.Tx) error {
		rows, err := tx.Query("SELECT DISTINCT pid FROM plink WHERE isprim=1 ORDER BY pid")
		if err != nil {
			return err
		}
		var pids []libfossil.FslID
		for rows.Next() {
			var pid int64
			if err := rows.Scan(&pid); err != nil {
				rows.Close()
				return err
			}
			pids = append(pids, libfossil.FslID(pid))
		}
		rows.Close()
		for _, pid := range pids {
			if err := tag.PropagateAllLeaves(tx, pid, leaf.NewPending()); err != nil {
				return err
			}
		}
		after = tagDigest(t, tx)
		return errRolledBack
	})
	if !errors.Is(err, errRolledBack) {
		t.Fatalf("%s: propagation probe: %v", where, err)
	}
	if before != after {
		t.Fatalf("%s: committed plink edges whose tags were never propagated:\n have %s\n owe  %s",
			where, before, after)
	}
}

// failPropagation makes the gate's propagation step fail until the returned
// func is called.
func failPropagation(t *testing.T) (restore func()) {
	t.Helper()
	orig := propagateAllLeaves
	boom := errors.New("injected propagation failure")
	propagateAllLeaves = func(db.Querier, libfossil.FslID, *leaf.Pending) error { return boom }
	restore = func() { propagateAllLeaves = orig }
	t.Cleanup(restore)
	return restore
}

// A link batch propagates from its parents before it commits, as fossil's
// manifest_crosslink does as it links each check-in: no committed state holds
// a plink edge whose parent's tags were not propagated, even before the
// sweep's closing gate runs.
func TestSweepBatchCommitsPropagated(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	c1 := storeCheckin(t, r, "c1", "trunk", base)
	storeCheckin(t, r, "c2", "", base.Add(time.Minute), c1)

	st := newLinkState(crosslinkCacheBytes)
	linked, err := crosslinkSweepWithState(context.Background(), r, st)
	if err != nil || linked != 2 {
		t.Fatalf("sweep without its closing gate: linked=%d err=%v", linked, err)
	}
	assertFullyPropagated(t, r, "after the batch commits")
	assertCheckinTags(t, r, "c2", []string{"branch=trunk/2"})
}

// A batch whose propagation fails rolls its links back with it, so nothing is
// committed that owes propagation, and a later run converges.
func TestSweepBatchRollsBackLinksWhenPropagationFails(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	c1 := storeCheckin(t, r, "c1", "trunk", base)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c1: %v", err)
	}
	c2 := storeCheckin(t, r, "c2", "", base.Add(time.Minute), c1)

	restore := failPropagation(t)
	if _, err := Crosslink(r); err == nil {
		t.Fatal("Crosslink with failing propagation succeeded")
	}
	assertFullyPropagated(t, r, "after the failed batch")
	var events int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?",
		ridOf(t, r, c2)).Scan(&events); err != nil || events != 0 {
		t.Fatalf("c2 event rows after the failed batch = %d, %v; want 0", events, err)
	}

	restore()
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink re-run: %v", err)
	}
	assertFullyPropagated(t, r, "after the re-run")
	assertCheckinTags(t, r, "c2", []string{"branch=trunk/2"})
	assertMatchesFullRebuild(t, r)
}

// A receive transaction propagates from the parents it linked before it
// commits; one whose propagation fails rolls back with its parents.
func TestReceiveLinkerWithTxCommitsPropagated(t *testing.T) {
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
	link := func(tx *db.Tx) error {
		return linker.LinkStored(context.Background(), tx, rid, c2, body)
	}

	restore := failPropagation(t)
	if err := linker.WithTx(r, link); err == nil {
		t.Fatal("WithTx with failing propagation succeeded")
	}
	assertFullyPropagated(t, r, "after the failed transaction")
	if got := linker.state.sched.parents.order; len(got) != 0 {
		t.Fatalf("parents after a rolled-back transaction = %v, want none", got)
	}

	// The re-run is a new receive session: the failed one's linker keeps the
	// blob as settled and leaves it to its Finalize sweep.
	restore()
	if linker, err = NewReceiveLinker(r); err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}
	if err := linker.WithTx(r, link); err != nil {
		t.Fatalf("WithTx re-run: %v", err)
	}
	assertFullyPropagated(t, r, "after the transaction commits, before Finalize")
	assertCheckinTags(t, r, "c2", []string{"branch=trunk/2"})
	if _, err := linker.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	assertMatchesFullRebuild(t, r)
}

// assertLeavesComplete fails if the committed leaf table differs from the one
// the leaf rule derives from the committed plink and branch rows: a linked
// check-in missing its leaf row, or a row the rule no longer admits. The probe
// runs in a transaction it rolls back.
func assertLeavesComplete(t *testing.T, r *repo.Repo, where string) {
	t.Helper()
	_, before := tagLeafState(t, r)
	var after string
	err := r.WithTx(func(tx *db.Tx) error {
		if err := leaf.Rebuild(tx); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT coalesce(group_concat(rid, '|'), '') FROM
		    (SELECT rid FROM leaf ORDER BY rid)`).Scan(&after); err != nil {
			return err
		}
		return errRolledBack
	})
	if !errors.Is(err, errRolledBack) {
		t.Fatalf("%s: leaf probe: %v", where, err)
	}
	if before != after {
		t.Fatalf("%s: committed leaf rows %q, the leaf rule derives %q", where, before, after)
	}
}

// failLeafInserts makes every insert into leaf fail, so a batch's leaf
// rechecks fail after its link writes and its propagation, before it commits,
// until the returned func is called.
func failLeafInserts(t *testing.T, r *repo.Repo) (restore func()) {
	t.Helper()
	if _, err := r.DB().Exec(`CREATE TRIGGER fail_leaf_insert BEFORE INSERT ON leaf
	    BEGIN SELECT RAISE(ABORT, 'injected leaf failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	dropped := false
	restore = func() {
		if dropped {
			return
		}
		dropped = true
		if _, err := r.DB().Exec("DROP TRIGGER fail_leaf_insert"); err != nil {
			t.Fatalf("drop trigger: %v", err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// A link batch applies its leaf rechecks before it commits, as fossil's
// manifest_crosslink_end runs leaf_do_pending_checks inside the transaction
// that linked the check-ins: a process that stops after a sweep's batches
// commit leaves no linked check-in without its leaf row.
func TestSweepBatchCommitsLeafRows(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	c1 := storeCheckin(t, r, "c1", "trunk", base)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c1: %v", err)
	}
	c2 := storeCheckin(t, r, "c2", "", base.Add(time.Minute), c1)

	// The sweep's batches, with nothing after them.
	st := newLinkState(crosslinkCacheBytes)
	linked, err := crosslinkSweepWithState(context.Background(), r, st)
	if err != nil || linked != 1 {
		t.Fatalf("sweep: linked=%d err=%v", linked, err)
	}
	if st.sched.owed || st.sched.leaves.Len() != 0 || len(st.sched.parents.order) != 0 {
		t.Fatalf("sweep left work for a later gate: owed=%v leaves=%d parents=%v",
			st.sched.owed, st.sched.leaves.Len(), st.sched.parents.order)
	}
	assertLeavesComplete(t, r, "after the batch commits")
	_, leaves := tagLeafState(t, r)
	if want := fmt.Sprint(ridOf(t, r, c2)); leaves != want {
		t.Fatalf("leaf rows = %q, want only c2 (%s)", leaves, want)
	}
}

// A batch whose leaf rechecks fail rolls its links back with them, so no
// committed check-in is left without its leaf row, and a later run converges
// on what a rebuild derives.
func TestSweepBatchRollsBackLinksWhenLeafCheckFails(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	c1 := storeCheckin(t, r, "c1", "trunk", base)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink c1: %v", err)
	}
	c2 := storeCheckin(t, r, "c2", "", base.Add(time.Minute), c1)
	storeCheckin(t, r, "c3", "", base.Add(2*time.Minute), c2)

	restore := failLeafInserts(t, r)
	_, err := Crosslink(r)
	restore()
	if err == nil {
		t.Fatal("Crosslink with failing leaf rechecks succeeded")
	}
	assertLeavesComplete(t, r, "after the failed batch")
	assertFullyPropagated(t, r, "after the failed batch")
	var events int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?",
		ridOf(t, r, c2)).Scan(&events); err != nil || events != 0 {
		t.Fatalf("c2 event rows after the failed batch = %d, %v; want 0", events, err)
	}

	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink re-run: %v", err)
	}
	assertLeavesComplete(t, r, "after the re-run")
	assertMatchesFullRebuild(t, r)
}
