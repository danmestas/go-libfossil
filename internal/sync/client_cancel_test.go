package sync

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
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// pollCancelCtx reports live for its first livePolls calls to Done() and
// cancelled from then on, so a test can place cancellation at one exact context
// poll. That precision is what makes a mid-walk assertion meaningful: the
// phantom-fill cascade (afterDephantomize) polls its context once per
// crosslinkCancelCheckStride work-stack pops, so a context that only dies on the
// second poll lets the walk crosslink the first stride's worth of children and
// then stop -- crosslinking some, but not all. Mirrors the manifest package's
// own newPollCancelCtx used by #166's TestAfterDephantomizeObservesCancellationMidWalk.
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

// buildPhantomFillFanout stores fanout distinct checkin-manifest blobs, each a
// depth-1 delta against a single base blob, then re-phantomizes that base so it
// reads as an unfilled phantom with fanout delta children waiting on it. It
// returns the base's UUID and full content -- deliver that via processResponse
// to fill the phantom -- plus the children's rids.
//
// When the base fills, afterDephantomize pops the base and pushes all fanout
// children (fanout+1 stack pops, deliberately past crosslinkCancelCheckStride),
// crosslinking each in turn. This is the same shape as #166's manifest-level
// fixture (buildDephantomizeFanout): the children are built with blob.StoreDelta
// against a real base -- the encoding content.Expand reconstructs cleanly -- and
// the base is turned back into a phantom afterward so filling it through the sync
// receive path is what triggers the cascade.
func buildPhantomFillFanout(t *testing.T, r *repo.Repo, fanout int) (baseUUID string, base []byte, children []libfossil.FslID) {
	t.Helper()

	// One shared file blob keeps the fixture cheap; the manifests differ only by
	// commit message and time, which is enough to make each a distinct blob.
	fileContent := []byte("phantom-fill fanout shared file")
	_, fileUUID, err := blob.Store(r.DB(), fileContent)
	if err != nil {
		t.Fatalf("Store fanout file blob: %v", err)
	}

	manifest := func(i int) []byte {
		d := &deck.Deck{
			Type: deck.Checkin,
			C:    fmt.Sprintf("phantom-fill fanout commit %d", i),
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

	// The base is itself checkin manifest(0); every child is a depth-1 delta
	// against it. A manifest base is what content.Expand reconstructs cleanly
	// through the delta encoding (buildDephantomizeFanout relies on the same
	// shape), so the fanout is fully walkable rather than tripping the blob
	// layer's delta round-trip on arbitrary source bytes.
	base = manifest(0)
	baseRid, baseUUID, err := blob.Store(r.DB(), base)
	if err != nil {
		t.Fatalf("Store fanout base blob: %v", err)
	}

	children = make([]libfossil.FslID, fanout)
	for i := range fanout {
		rid, _, err := blob.StoreDelta(r.DB(), manifest(i+1), baseRid)
		if err != nil {
			t.Fatalf("StoreDelta fanout child %d: %v", i, err)
		}
		children[i] = rid
	}

	// Re-phantomize the base: a phantom is a blob row with size = -1 and NULL
	// content plus a phantom-table row (see storeResolvedContent's fill path).
	// The delta children keep their delta.srcid link to this rid, so filling the
	// base through processResponse below is what dephantomizes it and cascades to
	// the children -- exactly the mid-round fill an ordinary sync performs.
	if _, err := r.DB().Exec("UPDATE blob SET size=-1, content=NULL WHERE rid=?", baseRid); err != nil {
		t.Fatalf("re-phantomize base (blob): %v", err)
	}
	if _, err := r.DB().Exec("INSERT OR IGNORE INTO phantom(rid) VALUES(?)", baseRid); err != nil {
		t.Fatalf("re-phantomize base (phantom table): %v", err)
	}
	return baseUUID, base, children
}

// countCrosslinkedRids reports how many of rids got a crosslink event row, i.e.
// how far the phantom-fill cascade actually processed.
func countCrosslinkedRids(t *testing.T, r *repo.Repo, rids []libfossil.FslID) int {
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

// TestProcessResponseThreadsContextToPhantomFillCascade pins issue #167: the
// general push/pull sync path's own deadline context must reach the mid-round
// phantom-fill crosslink cascade, the way #166 already made it reach the clone
// and server-push paths. session.Sync holds that context and hands it to
// processResponse, which now threads it through handleFileCard into
// storeReceivedFile's AfterDephantomize call -- where before #167 that call was
// hardcoded to context.Background() and no caller deadline could interrupt it.
//
// This is a threading test, not a re-test of the cascade's own cancellation
// logic (#166's TestAfterDephantomizeObservesCancellationMidWalk covers that):
// it proves the context handed to processResponse actually arrives at the
// cascade at all. fanout is spelled as a literal larger than
// crosslinkCancelCheckStride (256) so the walk polls its context more than once;
// a corpus derived from the stride could mask a neutered check.
func TestProcessResponseThreadsContextToPhantomFillCascade(t *testing.T) {
	const fanout = 300

	// Control: a live context fills the phantom and crosslinks every child,
	// proving the fixture is fully walkable through processResponse, so any
	// shortfall in the cancelled run below is the deadline biting, not a broken
	// graph or a receive path that never triggers the cascade.
	sLive, rLive := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})
	liveBaseUUID, liveBase, liveChildren := buildPhantomFillFanout(t, rLive, fanout)
	liveResp := &xfer.Message{Cards: []xfer.Card{&xfer.FileCard{UUID: liveBaseUUID, Content: liveBase}}}
	if _, err := sLive.processResponse(context.Background(), liveResp); err != nil {
		t.Fatalf("processResponse(live) = %v, want nil", err)
	}
	if got := countCrosslinkedRids(t, rLive, liveChildren); got != fanout {
		t.Fatalf("with a live context, crosslinked %d of %d fanout children through "+
			"processResponse, want all %d", got, fanout, fanout)
	}

	// Cancellation: the context is live for its first Done() poll -- the pop of
	// the base at stack position 0 -- and cancelled from the second poll on, the
	// pop at crosslinkCancelCheckStride. Because processResponse itself does not
	// poll the context, that first poll is the cascade's own; the base plus one
	// stride of children crosslink, then the walk observes the deadline and
	// stops. Against unmodified main -- where processResponse takes no context
	// and handleFileCard passes context.Background() to storeReceivedFile -- the
	// cascade never sees the cancellation and crosslinks all fanout children,
	// failing this assertion.
	sCancel, rCancel := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})
	cancelBaseUUID, cancelBase, cancelChildren := buildPhantomFillFanout(t, rCancel, fanout)
	cancelResp := &xfer.Message{Cards: []xfer.Card{&xfer.FileCard{UUID: cancelBaseUUID, Content: cancelBase}}}
	ctx := newPollCancelCtx(1)
	if _, err := sCancel.processResponse(ctx, cancelResp); err != nil {
		t.Fatalf("processResponse(cancel) = %v, want nil (the fill itself must not "+
			"error, only the cascade under it must stop early)", err)
	}
	if got := countCrosslinkedRids(t, rCancel, cancelChildren); got >= fanout {
		t.Fatalf("with a context cancelled mid-cascade, crosslinked %d of %d fanout "+
			"children: session.Sync's context did not reach the mid-round phantom-fill "+
			"cascade through processResponse (issue #167)", got, fanout)
	}
}
