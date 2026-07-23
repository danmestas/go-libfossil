package manifest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
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
