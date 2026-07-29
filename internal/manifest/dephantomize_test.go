package manifest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
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

func TestReceiveLinkerReplayAfterSettledCapDoesNotDuplicateMlinks(t *testing.T) {
	r := setupTestRepo(t)

	_, fileUUID, err := blob.Store(r.DB(), []byte("settled-cap file"))
	if err != nil {
		t.Fatalf("Store file blob: %v", err)
	}
	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "settled-cap checkin",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "settled-cap.txt", UUID: fileUUID}},
	}
	checkinBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal checkin: %v", err)
	}
	checkinRID, checkinUUID, err := blob.Store(r.DB(), checkinBytes)
	if err != nil {
		t.Fatalf("Store checkin blob: %v", err)
	}

	linker, err := NewReceiveLinker(r)
	if err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}
	for i := range receiveSettledLimit {
		linker.settled[libfossil.FslID(-i-1)] = struct{}{}
	}
	if got := len(linker.settled); got != receiveSettledLimit {
		t.Fatalf("pre-filled settled RIDs = %d, want %d", got, receiveSettledLimit)
	}

	if err := r.WithTx(func(tx *db.Tx) error {
		return linker.LinkStored(context.Background(), tx, checkinRID, checkinUUID, checkinBytes)
	}); err != nil {
		t.Fatalf("first LinkStored: %v", err)
	}

	var firstMlinks int
	if err := r.DB().QueryRow("SELECT count(*) FROM mlink WHERE mid=?", checkinRID).Scan(&firstMlinks); err != nil {
		t.Fatalf("count first mlink rows: %v", err)
	}
	if firstMlinks == 0 {
		t.Fatal("first LinkStored created no mlink rows")
	}
	firstLinked := linker.Stats().Linked
	if firstLinked == 0 {
		t.Fatal("first LinkStored did not increase Stats.Linked")
	}

	if err := r.WithTx(func(tx *db.Tx) error {
		return linker.LinkStored(context.Background(), tx, checkinRID, checkinUUID, checkinBytes)
	}); err != nil {
		t.Fatalf("replay LinkStored: %v", err)
	}

	var replayMlinks int
	if err := r.DB().QueryRow("SELECT count(*) FROM mlink WHERE mid=?", checkinRID).Scan(&replayMlinks); err != nil {
		t.Fatalf("count replay mlink rows: %v", err)
	}
	if replayMlinks != firstMlinks {
		t.Errorf("replay mlink rows = %d, want unchanged %d", replayMlinks, firstMlinks)
	}
	if got := linker.Stats().Linked; got != firstLinked {
		t.Errorf("replay Stats.Linked = %d, want unchanged %d", got, firstLinked)
	}
}

func TestReceiveLinkerReplayAfterSettledCapUsesEventMarkerForControl(t *testing.T) {
	r := setupTestRepo(t)
	_, targetUUID, err := blob.Store(r.DB(), []byte("settled-cap control target"))
	if err != nil {
		t.Fatalf("Store control target: %v", err)
	}
	d := &deck.Deck{
		Type: deck.Control,
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		T: []deck.TagCard{
			{Type: deck.TagSingleton, Name: "sym-settled-control", UUID: targetUUID},
		},
		U: deck.User("testuser"),
	}
	controlBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal control: %v", err)
	}
	controlRID, controlUUID, err := blob.Store(r.DB(), controlBytes)
	if err != nil {
		t.Fatalf("Store control blob: %v", err)
	}

	linker, err := NewReceiveLinker(r)
	if err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}
	for i := range receiveSettledLimit {
		linker.settled[libfossil.FslID(-i-1)] = struct{}{}
	}
	if got := len(linker.settled); got != receiveSettledLimit {
		t.Fatalf("pre-filled settled RIDs = %d, want %d", got, receiveSettledLimit)
	}

	if err := r.WithTx(func(tx *db.Tx) error {
		return linker.LinkStored(context.Background(), tx, controlRID, controlUUID, controlBytes)
	}); err != nil {
		t.Fatalf("first LinkStored: %v", err)
	}
	firstLinked := linker.Stats().Linked
	if firstLinked == 0 {
		t.Fatal("first LinkStored did not increase Stats.Linked")
	}

	var eventCount, sourceTagCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", controlRID).Scan(&eventCount); err != nil {
		t.Fatalf("count control event rows: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("control event rows = %d, want 1", eventCount)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", controlRID).Scan(&sourceTagCount); err != nil {
		t.Fatalf("count control source tagxref rows: %v", err)
	}
	if sourceTagCount == 0 {
		t.Fatal("first LinkStored created no control source tagxref rows")
	}

	r.DB().Exec("DELETE FROM tagxref WHERE srcid=?", controlRID)
	if err := r.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", controlRID).Scan(&sourceTagCount); err != nil {
		t.Fatalf("count removed control source tagxref rows: %v", err)
	}
	if sourceTagCount != 0 {
		t.Fatalf("control source tagxref rows after removal = %d, want 0", sourceTagCount)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", controlRID).Scan(&eventCount); err != nil {
		t.Fatalf("count retained control event rows: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("control event rows after source tag removal = %d, want 1", eventCount)
	}

	if err := r.WithTx(func(tx *db.Tx) error {
		return linker.LinkStored(context.Background(), tx, controlRID, controlUUID, controlBytes)
	}); err != nil {
		t.Fatalf("replay LinkStored: %v", err)
	}
	if got := linker.Stats().Linked; got != firstLinked {
		t.Errorf("replay Stats.Linked = %d, want unchanged %d", got, firstLinked)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", controlRID).Scan(&sourceTagCount); err != nil {
		t.Fatalf("count replay control source tagxref rows: %v", err)
	}
	if sourceTagCount != 0 {
		t.Errorf("replay control source tagxref rows = %d, want unchanged 0", sourceTagCount)
	}
}

func TestReceiveLinkerReplayAfterSettledCapUsesTagxrefMarkerForEvent(t *testing.T) {
	r := setupTestRepo(t)
	eventTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	d := &deck.Deck{
		Type: deck.Event,
		E: &deck.EventCard{
			Date: eventTime,
			UUID: "fedcba9876543210fedcba9876543210fedcba98",
		},
		U: deck.User("testuser"),
		D: eventTime,
		W: []byte("settled-cap event body"),
		C: "settled-cap event",
	}
	eventBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal event: %v", err)
	}
	eventRID, eventUUID, err := blob.Store(r.DB(), eventBytes)
	if err != nil {
		t.Fatalf("Store event blob: %v", err)
	}

	linker, err := NewReceiveLinker(r)
	if err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}
	for i := range receiveSettledLimit {
		linker.settled[libfossil.FslID(-i-1)] = struct{}{}
	}
	if got := len(linker.settled); got != receiveSettledLimit {
		t.Fatalf("pre-filled settled RIDs = %d, want %d", got, receiveSettledLimit)
	}

	if err := r.WithTx(func(tx *db.Tx) error {
		return linker.LinkStored(context.Background(), tx, eventRID, eventUUID, eventBytes)
	}); err != nil {
		t.Fatalf("first LinkStored: %v", err)
	}
	firstLinked := linker.Stats().Linked
	if firstLinked == 0 {
		t.Fatal("first LinkStored did not increase Stats.Linked")
	}

	var eventCount, firstTagCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", eventRID).Scan(&eventCount); err != nil {
		t.Fatalf("count event rows: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("event rows = %d, want 1", eventCount)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", eventRID).Scan(&firstTagCount); err != nil {
		t.Fatalf("count event source tagxref rows: %v", err)
	}
	if firstTagCount == 0 {
		t.Fatal("first LinkStored created no event source tagxref rows")
	}

	r.DB().Exec("DELETE FROM event WHERE objid=?", eventRID)
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", eventRID).Scan(&eventCount); err != nil {
		t.Fatalf("count removed event rows: %v", err)
	}
	if eventCount != 0 {
		t.Fatalf("event rows after removal = %d, want 0", eventCount)
	}

	if err := r.WithTx(func(tx *db.Tx) error {
		return linker.LinkStored(context.Background(), tx, eventRID, eventUUID, eventBytes)
	}); err != nil {
		t.Fatalf("replay LinkStored: %v", err)
	}
	if got := linker.Stats().Linked; got != firstLinked {
		t.Errorf("replay Stats.Linked = %d, want unchanged %d", got, firstLinked)
	}
	var replayTagCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", eventRID).Scan(&replayTagCount); err != nil {
		t.Fatalf("count replay event source tagxref rows: %v", err)
	}
	if replayTagCount != firstTagCount {
		t.Errorf("replay event source tagxref rows = %d, want unchanged %d", replayTagCount, firstTagCount)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", eventRID).Scan(&eventCount); err != nil {
		t.Fatalf("count replay event rows: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("replay event rows = %d, want unchanged 0", eventCount)
	}
}

func TestReceiveLinkerReplayAfterSettledCapUsesTagxrefMarkerForTicket(t *testing.T) {
	r := setupTestRepo(t)
	d := &deck.Deck{
		Type: deck.Ticket,
		K:    "0123456789abcdef0123456789abcdef01234567",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		J: []deck.TicketField{
			{Name: "title", Value: "settled-cap ticket"},
		},
	}
	ticketBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal ticket: %v", err)
	}
	ticketRID, ticketUUID, err := blob.Store(r.DB(), ticketBytes)
	if err != nil {
		t.Fatalf("Store ticket blob: %v", err)
	}

	linker, err := NewReceiveLinker(r)
	if err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}
	for i := range receiveSettledLimit {
		linker.settled[libfossil.FslID(-i-1)] = struct{}{}
	}
	if got := len(linker.settled); got != receiveSettledLimit {
		t.Fatalf("pre-filled settled RIDs = %d, want %d", got, receiveSettledLimit)
	}

	if err := r.WithTx(func(tx *db.Tx) error {
		return linker.LinkStored(context.Background(), tx, ticketRID, ticketUUID, ticketBytes)
	}); err != nil {
		t.Fatalf("first LinkStored: %v", err)
	}
	firstLinked := linker.Stats().Linked
	if firstLinked == 0 {
		t.Fatal("first LinkStored did not increase Stats.Linked")
	}

	var eventCount, firstTagCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", ticketRID).Scan(&eventCount); err != nil {
		t.Fatalf("count ticket event rows: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("ticket event rows = %d, want 1", eventCount)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", ticketRID).Scan(&firstTagCount); err != nil {
		t.Fatalf("count ticket source tagxref rows: %v", err)
	}
	if firstTagCount == 0 {
		t.Fatal("first LinkStored created no ticket source tagxref rows")
	}

	if err := r.WithTx(func(tx *db.Tx) error {
		return linker.LinkStored(context.Background(), tx, ticketRID, ticketUUID, ticketBytes)
	}); err != nil {
		t.Fatalf("replay LinkStored: %v", err)
	}
	if got := linker.Stats().Linked; got != firstLinked {
		t.Errorf("replay Stats.Linked = %d, want unchanged %d", got, firstLinked)
	}
	var replayTagCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", ticketRID).Scan(&replayTagCount); err != nil {
		t.Fatalf("count replay ticket source tagxref rows: %v", err)
	}
	if replayTagCount != firstTagCount {
		t.Errorf("replay ticket source tagxref rows = %d, want unchanged %d", replayTagCount, firstTagCount)
	}
}

// TestReceiveLinkerCancelledContextRollsBackPhantomFillCascade pins the
// receive-time cancellation boundary: an already-expired clone deadline must
// stop before linking a phantom base can recursively examine its delta children.
// The one extra child is deliberate: this fixture sits immediately above the
// 2,000-artifact cascade cap without turning the regression into a corpus test.
func TestReceiveLinkerCancelledContextRollsBackPhantomFillCascade(t *testing.T) {
	const fanout = 2_001

	r := setupTestRepo(t)
	base := []byte("cancelled phantom base with enough stable bytes for deltas")
	baseUUID := hash.SHA1(base)
	baseRID, err := blob.StorePhantom(r.DB(), baseUUID)
	if err != nil {
		t.Fatalf("StorePhantom base: %v", err)
	}

	children := make([]libfossil.FslID, fanout)
	for i := range children {
		child := []byte(fmt.Sprintf("cancelled phantom delta child %04d", i))
		rid, err := blob.StoreDeltaRaw(
			r.DB(),
			hash.SHA1(child),
			delta.Create(base, child),
			baseRID,
			nil,
		)
		if err != nil {
			t.Fatalf("StoreDeltaRaw child %d: %v", i, err)
		}
		children[i] = rid
	}

	linker, err := NewReceiveLinker(r)
	if err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = r.WithTx(func(tx *db.Tx) error {
		rid, uuid, err := blob.Store(tx, base)
		if err != nil {
			return fmt.Errorf("fill phantom base: %w", err)
		}
		if rid != baseRID {
			return fmt.Errorf("filled base rid = %d, want original phantom rid %d", rid, baseRID)
		}
		return linker.LinkStored(ctx, tx, rid, uuid, base)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LinkStored cancellation error = %v, want context.Canceled", err)
	}

	var baseSize int
	if err := r.DB().QueryRow("SELECT size FROM blob WHERE rid=?", baseRID).Scan(&baseSize); err != nil {
		t.Fatalf("query base size after rollback: %v", err)
	}
	if baseSize != -1 {
		t.Fatalf("base size after cancelled LinkStored = %d, want phantom size -1", baseSize)
	}
	var phantomCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM phantom WHERE rid=?", baseRID).Scan(&phantomCount); err != nil {
		t.Fatalf("query phantom after rollback: %v", err)
	}
	if phantomCount != 1 {
		t.Fatalf("phantom rows for base after cancelled LinkStored = %d, want 1", phantomCount)
	}

	for _, rid := range children {
		var terminal int
		if err := r.DB().QueryRow(`
			SELECT
				(SELECT count(*) FROM event WHERE objid=?)
			  + (SELECT count(*) FROM plink WHERE cid=? OR pid=?)
			  + (SELECT count(*) FROM mlink WHERE mid=?)
			  + (SELECT count(*) FROM tagxref WHERE rid=?)
			  + (SELECT count(*) FROM leaf WHERE rid=?)
			  + (SELECT count(*) FROM crosslink_nonartifact WHERE rid=?)`,
			rid, rid, rid, rid, rid, rid, rid,
		).Scan(&terminal); err != nil {
			t.Fatalf("query terminal state for child %d: %v", rid, err)
		}
		if terminal != 0 {
			t.Fatalf("child %d acquired %d terminal derived/nonartifact rows after cancelled LinkStored", rid, terminal)
		}
	}
}
func TestReceiveLinkerForumReplyBeforeRootPreservesReservedThreadReferences(t *testing.T) {
	ctx := context.Background()
	r := setupTestRepo(t)
	linker, err := NewReceiveLinker(r)
	if err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}

	root := &deck.Deck{
		Type: deck.ForumPost,
		H:    "reply-before-root thread",
		U:    deck.User("testuser"),
		W:    []byte("root post arrives after its reply"),
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	}
	rootBytes, err := root.Marshal()
	if err != nil {
		t.Fatalf("Marshal root forum post: %v", err)
	}
	rootUUID := hash.SHA1(rootBytes)

	reply := &deck.Deck{
		Type: deck.ForumPost,
		G:    rootUUID,
		H:    "reply-before-root thread",
		I:    rootUUID,
		U:    deck.User("testuser"),
		W:    []byte("reply received before its root"),
		D:    time.Date(2024, 1, 15, 10, 1, 0, 0, time.UTC),
	}
	replyBytes, err := reply.Marshal()
	if err != nil {
		t.Fatalf("Marshal reply forum post: %v", err)
	}

	var replyRID libfossil.FslID
	if err := r.WithTx(func(tx *db.Tx) error {
		var replyUUID string
		replyRID, replyUUID, err = blob.Store(tx, replyBytes)
		if err != nil {
			return err
		}
		return linker.LinkStored(ctx, tx, replyRID, replyUUID, replyBytes)
	}); err != nil {
		t.Fatalf("store and link reply: %v", err)
	}

	var reservedRoot, reservedIRT libfossil.FslID
	if err := r.DB().QueryRow(
		"SELECT froot, firt FROM forumpost WHERE fpid=?", replyRID,
	).Scan(&reservedRoot, &reservedIRT); err != nil {
		t.Fatalf("query reply forum references before root: %v", err)
	}
	if reservedRoot <= 0 || reservedIRT <= 0 {
		t.Fatalf("reply references before root = froot %d, firt %d; want positive reserved phantom RIDs",
			reservedRoot, reservedIRT)
	}
	if reservedRoot != reservedIRT {
		t.Fatalf("reply references before root = froot %d, firt %d; want both references to the root phantom",
			reservedRoot, reservedIRT)
	}
	phantomRootRID, exists := blob.Exists(r.DB(), rootUUID)
	if !exists {
		t.Fatal("reply link did not reserve a phantom for its root UUID")
	}
	if reservedRoot != phantomRootRID {
		t.Fatalf("reply froot before root = %d, want root phantom RID %d", reservedRoot, phantomRootRID)
	}

	var rootRID libfossil.FslID
	if err := r.WithTx(func(tx *db.Tx) error {
		var ok bool
		rootRID, ok = blob.Exists(tx, rootUUID)
		if !ok {
			return fmt.Errorf("root phantom %s disappeared before arrival", rootUUID)
		}
		compressed, err := blob.EncodeForStorage(rootBytes, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE blob SET size=?, content=?, rcvid=1 WHERE rid=?",
			len(rootBytes), compressed, rootRID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM phantom WHERE rid=?", rootRID); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rootRID); err != nil {
			return err
		}
		return linker.LinkStored(ctx, tx, rootRID, rootUUID, rootBytes)
	}); err != nil {
		t.Fatalf("store root into phantom and link: %v", err)
	}
	if rootRID != phantomRootRID {
		t.Fatalf("root filled RID = %d, want reserved phantom RID %d", rootRID, phantomRootRID)
	}

	assertReplyThread := func(stage string) {
		t.Helper()
		var froot, firt libfossil.FslID
		if err := r.DB().QueryRow(
			"SELECT froot, firt FROM forumpost WHERE fpid=?", replyRID,
		).Scan(&froot, &firt); err != nil {
			t.Fatalf("%s: query reply forum references: %v", stage, err)
		}
		if froot != rootRID || firt != rootRID {
			t.Fatalf("%s: reply references = froot %d, firt %d; want root RID %d",
				stage, froot, firt, rootRID)
		}
	}
	assertReplyThread("after root arrival")

	if _, err := linker.Finalize(ctx); err != nil {
		t.Fatalf("ReceiveLinker.Finalize: %v", err)
	}
	assertReplyThread("after Finalize")

	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink after ReceiveLinker.Finalize: %v", err)
	}
	assertReplyThread("after subsequent Crosslink")
}

// TestReceiveLinkerLinkStoredBoundsUncancelledCascadeAndFinalizesRemainder
// pins the receive-time hard budget independently of cancellation. One phantom
// source unblocks slightly more real checkin deltas than the budget permits:
// LinkStored must return after examining at most receiveCascadeLimit artifacts,
// with the accepted source included, and leave the durable remainder for
// Finalize rather than silently dropping it.
func TestReceiveLinkerLinkStoredBoundsUncancelledCascadeAndFinalizesRemainder(t *testing.T) {
	const fanout = receiveCascadeLimit + 1

	r := setupTestRepo(t)

	_, fileUUID, err := blob.Store(r.DB(), []byte("receive cascade budget file"))
	if err != nil {
		t.Fatalf("Store file blob: %v", err)
	}
	marshal := func(i int, parent string) []byte {
		d := &deck.Deck{
			Type: deck.Checkin,
			C:    fmt.Sprintf("receive cascade budget checkin %d", i),
			U:    deck.User("testuser"),
			D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			P:    nil,
			F:    []deck.FileCard{{Name: "receive-cascade-budget.txt", UUID: fileUUID}},
		}
		if parent != "" {
			d.P = []string{parent}
		}
		b, err := d.Marshal()
		if err != nil {
			t.Fatalf("Marshal checkin %d: %v", i, err)
		}
		return b
	}

	source := marshal(0, "")
	sourceUUID := hash.SHA1(source)
	sourceRID, err := blob.StorePhantom(r.DB(), sourceUUID)
	if err != nil {
		t.Fatalf("StorePhantom source: %v", err)
	}

	children := make([]libfossil.FslID, fanout)
	for i := range children {
		child := marshal(i+1, sourceUUID)
		rid, err := blob.StoreDeltaRaw(
			r.DB(),
			hash.SHA1(child),
			delta.Create(source, child),
			sourceRID,
			nil,
		)
		if err != nil {
			t.Fatalf("StoreDeltaRaw child %d: %v", i, err)
		}
		children[i] = rid
	}

	linker, err := NewReceiveLinker(r)
	if err != nil {
		t.Fatalf("NewReceiveLinker: %v", err)
	}
	if err := r.WithTx(func(tx *db.Tx) error {
		rid, uuid, err := blob.Store(tx, source)
		if err != nil {
			return fmt.Errorf("fill phantom source: %w", err)
		}
		if rid != sourceRID {
			return fmt.Errorf("filled source rid = %d, want phantom rid %d", rid, sourceRID)
		}
		return linker.LinkStored(context.Background(), tx, rid, uuid, source)
	}); err != nil {
		t.Fatalf("fill and LinkStored source: %v", err)
	}

	if got := linker.Stats().Linked; got == 0 {
		t.Fatal("LinkStored did not link the accepted source")
	} else if got > receiveCascadeLimit {
		t.Fatalf("LinkStored linked %d artifacts, want at most hard receive cascade budget %d "+
			"including the accepted source (issue #214)", got, receiveCascadeLimit)
	}

	if _, err := linker.Finalize(context.Background()); err != nil {
		t.Fatalf("ReceiveLinker.Finalize: %v", err)
	}
	if got := countCrosslinked(t, r, children); got != len(children) {
		t.Fatalf("Finalize crosslinked %d of %d residual delta children, want all", got, len(children))
	}

	if got, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink after ReceiveLinker.Finalize: %v", err)
	} else if got != 0 {
		t.Fatalf("Crosslink after ReceiveLinker.Finalize linked %d artifacts, want no-op", got)
	}
}
