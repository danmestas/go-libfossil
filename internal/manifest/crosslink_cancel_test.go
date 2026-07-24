package manifest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
)

// pollCancelCtx is a context that reports live for its first livePolls calls to
// Done() and cancelled from then on. The sweep polls its context once every
// crosslinkCancelCheckStride candidates, so a context that survives the first
// poll and dies before the second can only be observed by the *batched* check
// -- the i==0 check alone can never see it. That is what makes the test below
// discriminate: neuter the stride and this context is never sampled a second
// time, so the sweep runs to completion and the test fails.
type pollCancelCtx struct {
	context.Context
	livePolls int
	polls     int
	live      chan struct{} // never closed
	dead      chan struct{} // closed at construction
}

func newPollCancelCtx(livePolls int) *pollCancelCtx {
	dead := make(chan struct{})
	close(dead)
	return &pollCancelCtx{
		Context:   context.Background(),
		livePolls: livePolls,
		live:      make(chan struct{}),
		dead:      dead,
	}
}

func (c *pollCancelCtx) Done() <-chan struct{} {
	c.polls++
	if c.polls <= c.livePolls {
		return c.live
	}
	return c.dead
}

func (c *pollCancelCtx) Err() error {
	if c.polls <= c.livePolls {
		return nil
	}
	return context.Canceled
}

// TestCrosslinkContextObservesCancellationMidSweep pins that the crosslink
// sweep's *batched* cancellation check actually fires. Crosslink is the one
// phase of a clone that walks the whole received repository in a single call
// with no round boundary to fall back on, so without an in-loop check a clone
// deadline cannot interrupt it -- the "ran long past its deadline, never
// completed" symptom of #120 at large-repository scale.
//
// The corpus is deliberately larger than crosslinkCancelCheckStride so the
// sweep polls its context more than once, and the context is live for the first
// poll (i == 0) and cancelled for the second (i == stride). A sweep that only
// checked at i == 0 -- or whose stride were large enough to never sample again
// -- would run to completion and return a nil error here.
func TestCrosslinkContextObservesCancellationMidSweep(t *testing.T) {
	r := setupTestRepo(t)

	// Comfortably more than crosslinkCancelCheckStride, so the sweep polls its
	// context at least twice. Spelled as a literal rather than derived from the
	// stride on purpose: a corpus that shrank or grew in lockstep with the
	// stride could never detect the stride being neutered.
	const candidates = 768
	for i := range candidates {
		if _, _, err := blob.Store(r.DB(), fmt.Appendf(nil, "candidate blob %d, not a manifest", i)); err != nil {
			t.Fatalf("blob.Store(%d): %v", i, err)
		}
	}

	ctx := newPollCancelCtx(1)

	linked, err := CrosslinkContext(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CrosslinkContext error = %v, want context.Canceled "+
			"(the batched in-loop check did not fire; polls=%d)", err, ctx.polls)
	}
	if ctx.polls < 2 {
		t.Errorf("context polled %d time(s); the sweep must sample it more than "+
			"once for the batched check to mean anything", ctx.polls)
	}
	if linked != 0 {
		t.Errorf("linked = %d, want 0 (no candidate here is a real manifest)", linked)
	}
}

// TestCrosslinkContextObservesPreCancelledContext keeps the cheap i == 0 case
// honest: an already-cancelled context aborts before any work is done.
func TestCrosslinkContextObservesPreCancelledContext(t *testing.T) {
	r := setupTestRepo(t)

	if _, _, err := blob.Store(r.DB(), []byte("candidate blob, not a manifest")); err != nil {
		t.Fatalf("blob.Store: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	linked, err := CrosslinkContext(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CrosslinkContext error = %v, want context.Canceled", err)
	}
	if linked != 0 {
		t.Errorf("linked = %d, want 0 on immediate cancellation", linked)
	}
}

// TestCrosslinkRepairsLeafAfterCancelledSweep pins issue #143: a sweep that is
// cancelled after committing at least one batch must still run the leaf/tag
// repair pass before returning, rather than leaving derived leaf/tag state
// stale until some later sweep happens to link an artifact.
//
// Method: build one real root check-in (childless, so it is a leaf) plus enough
// filler blobs to push the candidate set past a single batch. leaf is populated
// only by the deferred repair pass -- crosslinkCheckinTables never touches it --
// so a check-in that is committed but whose sweep skips the repair never lands
// in leaf. Cancel at the second batch's boundary, with the first batch (holding
// the check-in) already committed: the only path that returns linked > 0
// together with a context error, since a mid-batch cancellation rolls its batch
// back and commits nothing. Then run a second, zero-link sweep and confirm the
// repaired leaf state persists across it rather than the stale window reopening.
func TestCrosslinkRepairsLeafAfterCancelledSweep(t *testing.T) {
	r := setupTestRepo(t)
	d := r.DB()

	_, fileUUID, err := blob.Store(d, []byte("root file content"))
	if err != nil {
		t.Fatalf("blob.Store file: %v", err)
	}
	ci := &deck.Deck{
		Type: deck.Checkin,
		C:    "root checkin",
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		U:    deck.User("testuser"),
		F:    []deck.FileCard{{Name: "file.txt", UUID: fileUUID}},
		R:    "0000000000000000000000000000000000000000",
	}
	ciBytes, err := ci.Marshal()
	if err != nil {
		t.Fatalf("Marshal checkin: %v", err)
	}
	checkinRid, _, err := blob.Store(d, ciBytes)
	if err != nil {
		t.Fatalf("blob.Store checkin: %v", err)
	}

	// Pad past one batch so the sweep reaches a second batch boundary. The
	// check-in and its file blob sit at the front by rid, well inside batch 0.
	for i := 0; i < crosslinkBatchSize-1; i++ {
		if _, _, err := blob.Store(d, fmt.Appendf(nil, "filler %d not a manifest", i)); err != nil {
			t.Fatalf("blob.Store filler %d: %v", i, err)
		}
	}

	// Stay live through the whole first batch, die at the second batch's
	// boundary poll: one boundary poll before batch 0, then one in-batch poll
	// per stride within it. Derived from the constants so it tracks any
	// retuning of either the batch size or the stride.
	inBatchPolls := (crosslinkBatchSize + crosslinkCancelCheckStride - 1) / crosslinkCancelCheckStride
	ctx := newPollCancelCtx(1 + inBatchPolls)

	linked, err := CrosslinkContext(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CrosslinkContext error = %v, want context.Canceled (polls=%d)", err, ctx.polls)
	}
	if linked < 1 {
		t.Fatalf("linked = %d, want >= 1 (the first batch commits the check-in before cancellation)", linked)
	}

	leafCount := func(when string) int {
		var n int
		if scanErr := d.QueryRow("SELECT count(*) FROM leaf WHERE rid=?", checkinRid).Scan(&n); scanErr != nil {
			t.Fatalf("leaf query %s: %v", when, scanErr)
		}
		return n
	}

	// The fix: the cancelled sweep still ran the leaf repair, so the childless
	// check-in is recorded as a leaf despite the interruption.
	if got := leafCount("after cancelled sweep"); got != 1 {
		t.Fatalf("leaf rows for rid=%d after cancelled sweep = %d, want 1 "+
			"(the cancelled sweep skipped the leaf repair, issue #143)", checkinRid, got)
	}

	// A later sweep whose remaining candidates all link zero artifacts skips
	// the repair gate itself; the state repaired above must already be correct
	// and simply persist, rather than staying stale until a future sweep links
	// something.
	n, err := Crosslink(r)
	if err != nil {
		t.Fatalf("second Crosslink: %v", err)
	}
	if n != 0 {
		t.Fatalf("second Crosslink linked = %d, want 0 (no remaining candidate is a manifest)", n)
	}
	if got := leafCount("after following zero-link sweep"); got != 1 {
		t.Fatalf("leaf rows for rid=%d after zero-link sweep = %d, want 1 "+
			"(stale leaf state persisted across the zero-link sweep)", checkinRid, got)
	}
}

// TestCrosslinkStillWorksWithoutContext keeps the historical Crosslink entry
// point honest: it must behave exactly as before, supplying its own background
// context, so the ~30 existing callers need no change.
func TestCrosslinkStillWorksWithoutContext(t *testing.T) {
	r := setupTestRepo(t)
	if _, _, err := blob.Store(r.DB(), []byte("candidate blob, not a manifest")); err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
}
