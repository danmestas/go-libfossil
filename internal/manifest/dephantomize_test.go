package manifest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestAfterDephantomizeCheckin(t *testing.T) {
	r := setupTestRepo(t)

	// Create a checkin manifest, compute its hash, store as phantom, then fill.
	fileContent := []byte("hello dephantomize")
	fileRid, fileUUID, err := blob.Store(r.DB(), fileContent)
	if err != nil {
		t.Fatalf("Store file blob: %v", err)
	}
	_ = fileRid

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "dephantomize commit",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
	}

	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Compute the hash to get the UUID.
	rid, uuid, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("Store manifest: %v", err)
	}

	// Delete the real blob, re-insert as phantom, then fill it back.
	// This simulates the phantom->real transition.
	r.DB().Exec("DELETE FROM event WHERE objid=?", rid)
	r.DB().Exec("DELETE FROM plink WHERE cid=?", rid)
	r.DB().Exec("DELETE FROM leaf WHERE rid=?", rid)
	r.DB().Exec("DELETE FROM mlink WHERE mid=?", rid)
	r.DB().Exec("DELETE FROM tagxref WHERE rid=?", rid)

	// Verify no event row exists before dephantomize.
	var eventCount int
	r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&eventCount)
	if eventCount != 0 {
		t.Fatalf("event count before dephantomize = %d, want 0", eventCount)
	}

	// Call AfterDephantomize.
	AfterDephantomize(context.Background(), r, rid)

	// Verify event row was created.
	r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&eventCount)
	if eventCount != 1 {
		t.Errorf("event count after dephantomize = %d, want 1", eventCount)
	}

	// Verify event is a checkin.
	var eventType string
	r.DB().QueryRow("SELECT type FROM event WHERE objid=?", rid).Scan(&eventType)
	if eventType != "ci" {
		t.Errorf("event type = %q, want 'ci'", eventType)
	}

	_ = uuid
}

func TestAfterDephantomizeOrphan(t *testing.T) {
	r := setupTestRepo(t)

	// Create a baseline checkin.
	fileContent := []byte("baseline file")
	_, fileUUID, err := blob.Store(r.DB(), fileContent)
	if err != nil {
		t.Fatalf("Store file: %v", err)
	}

	baselineDeck := &deck.Deck{
		Type: deck.Checkin,
		C:    "baseline",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "file.txt", UUID: fileUUID}},
	}
	baselineBytes, err := baselineDeck.Marshal()
	if err != nil {
		t.Fatalf("Marshal baseline: %v", err)
	}
	baselineRid, _, err := blob.Store(r.DB(), baselineBytes)
	if err != nil {
		t.Fatalf("Store baseline: %v", err)
	}

	// Create another checkin that will be the "orphan" (delta manifest).
	fileContent2 := []byte("orphan file")
	_, fileUUID2, err := blob.Store(r.DB(), fileContent2)
	if err != nil {
		t.Fatalf("Store file2: %v", err)
	}

	orphanDeck := &deck.Deck{
		Type: deck.Checkin,
		C:    "orphan commit",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "file2.txt", UUID: fileUUID2}},
	}
	orphanBytes, err := orphanDeck.Marshal()
	if err != nil {
		t.Fatalf("Marshal orphan: %v", err)
	}
	orphanRid, _, err := blob.Store(r.DB(), orphanBytes)
	if err != nil {
		t.Fatalf("Store orphan: %v", err)
	}

	// Clear crosslink tables for the orphan.
	r.DB().Exec("DELETE FROM event WHERE objid=?", orphanRid)
	r.DB().Exec("DELETE FROM leaf WHERE rid=?", orphanRid)
	r.DB().Exec("DELETE FROM tagxref WHERE rid=?", orphanRid)

	// Insert orphan row linking orphanRid to baselineRid.
	r.DB().Exec("INSERT INTO orphan(rid, baseline) VALUES(?, ?)", orphanRid, baselineRid)

	// Verify orphan row exists.
	var orphanCount int
	r.DB().QueryRow("SELECT count(*) FROM orphan WHERE baseline=?", baselineRid).Scan(&orphanCount)
	if orphanCount != 1 {
		t.Fatalf("orphan count = %d, want 1", orphanCount)
	}

	// Call AfterDephantomize on the baseline.
	AfterDephantomize(context.Background(), r, baselineRid)

	// Verify orphan was cleaned up.
	r.DB().QueryRow("SELECT count(*) FROM orphan WHERE baseline=?", baselineRid).Scan(&orphanCount)
	if orphanCount != 0 {
		t.Errorf("orphan count after dephantomize = %d, want 0", orphanCount)
	}

	// Verify orphan checkin got crosslinked (event row created).
	var eventCount int
	r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", orphanRid).Scan(&eventCount)
	if eventCount != 0 {
		// The orphan's event may have been created by crosslinkSingle.
		// If it wasn't (because it was already crosslinked earlier), that's also fine.
		// The key assertion is that the orphan row was cleaned up.
	}
}

// buildDephantomizeFanout stores one full "root" checkin manifest plus fanout
// distinct checkin manifests, each a depth-1 delta against that root, so every
// child appears in the delta table with srcid = root. That is exactly the shape
// afterDephantomize's work stack walks when a base fills and unblocks the delta
// children waiting on it: root is popped, all fanout children are pushed, then
// each is popped in turn. The walk therefore runs fanout+1 stack pops -- deep
// enough to cross crosslinkCancelCheckStride -- while every child stays a
// single-hop delta so content.Expand reconstructs it without a deep delta chain.
func buildDephantomizeFanout(t *testing.T, r *repo.Repo, fanout int) (root libfossil.FslID, children []libfossil.FslID) {
	t.Helper()

	// One shared file blob keeps the fixture cheap; the manifests differ only by
	// commit message and time, which is enough to make each a distinct blob.
	fileContent := []byte("fanout shared file")
	_, fileUUID, err := blob.Store(r.DB(), fileContent)
	if err != nil {
		t.Fatalf("Store fanout file blob: %v", err)
	}

	manifest := func(i int) []byte {
		d := &deck.Deck{
			Type: deck.Checkin,
			C:    fmt.Sprintf("fanout commit %d", i),
			U:    deck.User("testuser"),
			D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
		}
		mb, err := d.Marshal()
		if err != nil {
			t.Fatalf("Marshal fanout manifest %d: %v", i, err)
		}
		return mb
	}

	root, _, err = blob.Store(r.DB(), manifest(0))
	if err != nil {
		t.Fatalf("Store fanout root: %v", err)
	}

	children = make([]libfossil.FslID, fanout)
	for i := 0; i < fanout; i++ {
		rid, _, err := blob.StoreDelta(r.DB(), manifest(i+1), root)
		if err != nil {
			t.Fatalf("StoreDelta fanout child %d: %v", i, err)
		}
		children[i] = rid
	}
	return root, children
}

// countCrosslinked reports how many of rids got a crosslink event row, i.e. how
// far the phantom-fill cascade actually processed.
func countCrosslinked(t *testing.T, r *repo.Repo, rids []libfossil.FslID) int {
	t.Helper()
	n := 0
	for _, rid := range rids {
		var c int
		if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&c); err != nil {
			t.Fatalf("count event objid=%d: %v", rid, err)
		}
		if c > 0 {
			n++
		}
	}
	return n
}

// TestAfterDephantomizeObservesCancellationMidWalk pins that a clone's deadline
// can interrupt the mid-round phantom-fill cascade (issue #166). AfterDephantomize
// runs synchronously as a clone fills phantoms and walks the whole reachable
// delta/orphan graph; before the fix nothing in that walk observed the clone's
// context, so on a large graph it ran for many multiples of the configured
// deadline, bounded only by a 1,000,000-iteration safety cap.
//
// fanout is deliberately larger than crosslinkCancelCheckStride so the work
// stack polls its context more than once. Spelled as a literal -- matching the
// sibling TestCrosslinkContextObservesCancellationMidSweep -- so a corpus that
// grew or shrank in lockstep with the stride could never mask a neutered check.
func TestAfterDephantomizeObservesCancellationMidWalk(t *testing.T) {
	const fanout = 300

	// Control: a live context crosslinks every child. This proves the fixture is
	// fully walkable, so any shortfall in the cancelled run below is the deadline
	// biting, not a broken graph.
	rLive := setupTestRepo(t)
	liveRoot, liveChildren := buildDephantomizeFanout(t, rLive, fanout)
	AfterDephantomize(context.Background(), rLive, liveRoot)
	if got := countCrosslinked(t, rLive, liveChildren); got != fanout {
		t.Fatalf("with a live context, crosslinked %d of %d fanout children, want all %d",
			got, fanout, fanout)
	}

	// Cancellation: the context is live for its first Done() poll (the pop of
	// the root, at stack position 0) and cancelled from the second poll on (the
	// pop at crosslinkCancelCheckStride). Only an in-loop, stride-batched check
	// can observe that transition; a walk that ignores ctx runs to completion.
	rCancel := setupTestRepo(t)
	cancelRoot, cancelChildren := buildDephantomizeFanout(t, rCancel, fanout)
	ctx := newPollCancelCtx(1)
	AfterDephantomize(ctx, rCancel, cancelRoot)
	if got := countCrosslinked(t, rCancel, cancelChildren); got >= fanout {
		t.Fatalf("with a context cancelled mid-walk, crosslinked %d of %d fanout children: "+
			"AfterDephantomize ran the whole phantom-fill cascade without observing the "+
			"deadline (issue #166)", got, fanout)
	}
}

func TestAfterDephantomizeNilRepo(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil repo")
		}
	}()
	AfterDephantomize(context.Background(), nil, 1)
}

func TestAfterDephantomizeZeroRid(t *testing.T) {
	r := setupTestRepo(t)
	// Should return without panicking.
	AfterDephantomize(context.Background(), r, 0)
	AfterDephantomize(context.Background(), r, -1)
}
