package sync_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// endlessTransport delivers one fresh file card every round and never signals
// completion, so a clone against it converges only when its context is
// cancelled. It cancels that context itself after cancelAt rounds.
type endlessTransport struct {
	cancel   context.CancelFunc
	cancelAt int
	round    int
}

func (t *endlessTransport) Exchange(_ context.Context, _ *xfer.Message) (*xfer.Message, error) {
	t.round++
	if t.round >= t.cancelAt {
		t.cancel()
	}
	content := fmt.Appendf(nil, "endless blob %d", t.round)
	return &xfer.Message{Cards: []xfer.Card{
		&xfer.FileCard{UUID: hash.SHA1(content), Content: content},
	}}, nil
}

// TestCloneAbortsOnContextCancellation pins #120's core requirement: a cancelled
// context reliably aborts a clone within a small, bounded number of rounds --
// not "sometimes never". This one is satisfied by the round-loop check alone;
// the two tests below cover the cancellation points that check cannot reach.
func TestCloneAbortsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	const cancelAt = 3
	transport := &endlessTransport{cancel: cancel, cancelAt: cancelAt}

	path := filepath.Join(t.TempDir(), "clone.fossil")
	_, result, err := sync.Clone(ctx, path, transport, sync.CloneOpts{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("clone error = %v, want context.Canceled", err)
	}
	if result == nil {
		t.Fatal("result is nil; a cancelled clone should still report its progress")
	}
	// It must have stopped near the cancellation point, well short of the
	// round limit -- proving the deadline aborted it rather than the loop
	// simply exhausting its rounds.
	if result.Rounds >= sync.MaxRounds {
		t.Errorf("clone ran %d rounds (limit %d); cancellation did not abort it early",
			result.Rounds, sync.MaxRounds)
	}
}

// pollCancelCtx reports live for its first livePolls calls to Done() and
// cancelled from then on, which lets a test place cancellation at one exact
// context poll. That precision is what isolates processResponse's batched
// in-loop check from the round-loop check that precedes it: the round loop
// polls once per round and processResponse polls at card 0, so a context that
// only dies on the third poll can be seen by nothing but the check at card
// processResponseCancelCheckStride.
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

// bigBatchTransport answers the first round with cardCount distinct file cards
// and nothing afterwards, so a single processResponse call has a long run of
// cards to walk with no network wait in between.
type bigBatchTransport struct {
	cardCount int
	round     int
}

func (t *bigBatchTransport) Exchange(_ context.Context, _ *xfer.Message) (*xfer.Message, error) {
	t.round++
	if t.round > 1 {
		return &xfer.Message{}, nil
	}
	cards := make([]xfer.Card, 0, t.cardCount)
	for i := range t.cardCount {
		content := fmt.Appendf(nil, "batch blob %d", i)
		cards = append(cards, &xfer.FileCard{UUID: hash.SHA1(content), Content: content})
	}
	return &xfer.Message{Cards: cards}, nil
}

// TestCloneProcessResponseObservesCancellationMidBatch pins the cancellation
// check *inside* processResponse. One clone round is processed card by card
// with no network wait in between, so a large single-round batch would run to
// completion no matter how long ago the deadline expired -- the round-loop
// check at the top of the next cycle is too late.
//
// The context is live for the round-loop poll and for processResponse's card-0
// poll, and cancelled from the next poll on. Nothing but the batched check at
// card processResponseCancelCheckStride can observe it, so this fails both
// against unmodified main (where processResponse takes no context at all) and
// against a build whose stride is large enough to skip the second sample.
func TestCloneProcessResponseObservesCancellationMidBatch(t *testing.T) {
	// Comfortably more than processResponseCancelCheckStride (256). Spelled as
	// a literal, not derived from the stride, so neutering the stride cannot
	// silently resize the batch along with it.
	transport := &bigBatchTransport{cardCount: 768}

	// Poll 1: round-loop select. Poll 2: processResponse at card 0. Poll 3 --
	// the first dead one -- is the batched check partway through the batch.
	ctx := newPollCancelCtx(2)

	path := filepath.Join(t.TempDir(), "clone.fossil")
	_, _, err := sync.Clone(ctx, path, transport, sync.CloneOpts{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("clone error = %v, want context.Canceled (the in-batch check "+
			"did not fire; context polled %d times)", err, ctx.polls)
	}
	// The round loop wraps a processResponse failure with "process round"; a
	// bare round-loop abort is not wrapped that way. Asserting on the wrapper
	// is what proves the *new* check fired and not the pre-existing one.
	if !strings.Contains(err.Error(), "process round") {
		t.Fatalf("clone error = %v, want it to come from processResponse "+
			"(the round-loop check fired instead)", err)
	}
	if ctx.polls < 3 {
		t.Errorf("context polled %d time(s); processResponse must sample it "+
			"more than once per round for the batched check to mean anything",
			ctx.polls)
	}
}

// twoRoundTransport delivers one blob, then an empty message that ends the
// clone. It invokes onLastRound just before returning that final message, which
// puts a side effect exactly between the last round-loop poll and the
// post-loop crosslink sweep.
type twoRoundTransport struct {
	onLastRound func()
	round       int
}

func (t *twoRoundTransport) Exchange(_ context.Context, _ *xfer.Message) (*xfer.Message, error) {
	t.round++
	if t.round == 1 {
		content := []byte("only blob")
		// Seqno 0 in the same round as the payload, not in the final one: the
		// final response has to be genuinely empty for processResponse to poll
		// the context zero times, which is what leaves the crosslink sweep as
		// the only check that can see the cancellation.
		return &xfer.Message{Cards: []xfer.Card{
			&xfer.FileCard{UUID: hash.SHA1(content), Content: content},
			&xfer.CloneSeqNoCard{SeqNo: 0},
		}}, nil
	}
	if t.onLastRound != nil {
		t.onLastRound()
	}
	// No cards: processResponse walks nothing, so it polls the context zero
	// times and the clone falls out of the round loop into crosslink.
	return &xfer.Message{}, nil
}

// TestCloneCrosslinkSweepObservesCancellation pins the cancellation check in
// the whole-repository crosslink sweep. That sweep runs *after* the round loop
// has already exited, so it has no round boundary of its own at which a
// deadline could fire -- on a large repository it could run long past the
// deadline uninterrupted, which is exactly what #120 reported.
//
// Cancellation lands after the final round-loop poll and after a zero-card
// final response (which makes processResponse poll nothing), so the sweep's
// check is the only one left that can observe it. Against unmodified main --
// where Clone calls the context-free manifest.Crosslink -- this clone succeeds
// and the test fails.
func TestCloneCrosslinkSweepObservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transport := &twoRoundTransport{onLastRound: cancel}

	path := filepath.Join(t.TempDir(), "clone.fossil")
	_, _, err := sync.Clone(ctx, path, transport, sync.CloneOpts{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("clone error = %v, want context.Canceled from the crosslink sweep", err)
	}
	if !strings.Contains(err.Error(), "crosslink") {
		t.Fatalf("clone error = %v, want it to come from the crosslink sweep "+
			"(an earlier check fired instead)", err)
	}
}
