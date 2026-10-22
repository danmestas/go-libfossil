package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The merge-base query and deepen (draft-fossil-horizon-00 Appendix A step
// 6: §5.5, §5.7, §5.8, §5.9, §6.2.4) over hzStruct: go-libfossil's handler
// is the origin, its structural clone the client.

func horizonDeepenCases() []Case {
	return []Case{horizonMergeBaseQuery, horizonMergeBaseBound, horizonDeepen, horizonDeepenPoint, horizonDeepenFailed}
}

var (
	horizonMergeBaseQuery = Case{ID: "horizon.6.2.4.merge-base", Req: "HZ-6.2-7"}
	horizonMergeBaseBound = Case{ID: "horizon.8.merge-base-bound", Req: "HZ-8-1"}
	horizonDeepen         = Case{ID: "horizon.5.8.deepen", Req: "HZ-5.8-1"}
	horizonDeepenPoint    = Case{ID: "horizon.5.8.deepen-point", Req: "HZ-5.8-1"}
	horizonDeepenFailed   = Case{ID: "horizon.5.8.deepen-failed", Req: "HZ-5.8-1"}
)

func TestHorizonDeepen(t *testing.T) {
	check(t, horizonMergeBaseQuery, func() error { return hzMergeBaseQuery(t) })
	check(t, horizonMergeBaseBound, func() error { return hzMergeBaseBound(t) })
	check(t, horizonDeepen, func() error { return hzDeepen(t) })
	check(t, horizonDeepenPoint, func() error { return hzDeepenPoint(t) })
	check(t, horizonDeepenFailed, func() error { return hzDeepenFailed(t) })
}

// hzMergeBaseQuery: a structural clone asks its origin for the merge base of
// two check-ins, one below its horizon, and the origin answers from its own
// graph: the base, or - for check-ins sharing no ancestor.
func hzMergeBaseQuery(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	for _, q := range [][3]string{{x.c4, x.feat1, x.c1}, {x.feat2, x.c4, x.c3}} {
		res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true, MergeBaseOf: [2]string{q[0], q[1]}})
		if err != nil || !res.MergeBaseAnswered || res.MergeBase != q[2] {
			return expectf("base of %.10s and %.10s: %v answered=%v %q, want %.10s",
				q[0], q[1], err, res.MergeBaseAnswered, res.MergeBase, q[2])
		}
	}
	root := hzCommit(t, x.s.f, "unrelated", []libfossil.FslID{0}, nil)
	res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true,
		MergeBaseOf: [2]string{x.c4, hzUUID(t, x.s.f.r, root)}})
	if err != nil || !res.MergeBaseAnswered || res.MergeBase != "" {
		return expectf("unrelated: %v answered=%v %q, want -", err, res.MergeBaseAnswered, res.MergeBase)
	}
	return nil
}

// hzMergeBaseBound: the walk behind an answer stops at its visited-node
// bound, as the origin's other graph walks do (§8).
func hzMergeBaseBound(t *testing.T) error {
	x := newHZStruct(t)
	if _, err := merge.FindCommonAncestorWithin(context.Background(), x.s.f.r, x.s.c4, x.s.feat1, 2); !errors.Is(err, merge.ErrBound) {
		return expectf("a walk past its bound: %v, want merge.ErrBound", err)
	}
	base, err := merge.FindCommonAncestorWithin(context.Background(), x.s.f.r, x.s.c4, x.s.feat1, 64)
	if err != nil || base != x.s.c1 {
		return expectf("within the bound: %d %v, want c1", base, err)
	}
	return nil
}

// hzDeepen: deepening a structural clone from C = c3 to its parent c2 moves
// the stored horizon, discards the seed for c3 before the synchronization
// (the request offers none), takes c2's manifest and the seed for c2, and
// replaces nothing held: K's check-ins derive densely, only c1 and feat1
// stay below.
func hzDeepen(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	before, err := hzHeldNames(r)
	if err != nil {
		return err
	}
	peer := x.peer()
	res, err := sync.Deepen(context.Background(), r, peer, x.c2, sync.SyncOpts{})
	if err != nil || res.HorizonFallback || res.StructuralServed == 0 {
		return expectf("deepen to c2: %v %+v", err, res)
	}
	if err := hzStoredHorizon(r, x.c2, cone.DepthStructural); err != nil {
		return err
	}
	first := peer.sent()[0]
	if got := hzPragmas(first, "horizon-seed"); len(got) != 1 || got[0][0] != "-" {
		return expectf("the deepen's first request offers seed %q, want - (the seed for c3 discarded first)", got)
	}
	if got := hzPragmas(first, "horizon"); len(got) != 1 || got[0][0] != x.c2 {
		return expectf("the deepen's first request names horizon %q, want c2", got)
	}
	k, err := horizon.HeldSet(context.Background(), x.s.f.r.DB(), x.s.c2, cone.RuleDescendants)
	if err != nil {
		return err
	}
	_, want, err := seed.Mint(x.s.f.r.DB(), x.s.c2, k, seedLoginGo)
	if err != nil {
		return err
	}
	if got := mustHeld(t, r); string(got) != string(want) {
		return expectf("held seed after the deepen:\n%s\nwant the seed for c2:\n%s", got, want)
	}
	after, err := hzHeldNames(r)
	if err != nil {
		return err
	}
	for name := range before {
		if !after[name] {
			return expectf("the deepen dropped held artifact %.10s", name)
		}
	}
	if !after[x.c2] {
		return expectf("c2's manifest is not held after the deepen")
	}
	x.s.k = k
	if err := x.s.derivesDense(t, r); err != nil {
		return err
	}
	return hzElided(r, map[string]bool{x.c1: true, x.feat1: true})
}

// hzDeepenPoint: a deepen toward a check-in that is not an ancestor of C
// moves C to their merge base, so K only grows: feat1 lies off C's
// ancestry, the clone holds neither its manifest nor its ancestry, so it
// asks the origin, and C' is c1.
func hzDeepenPoint(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	peer := x.peer()
	if _, err := sync.Deepen(context.Background(), r, peer, x.feat1, sync.SyncOpts{}); err != nil {
		return err
	}
	if err := hzStoredHorizon(r, x.c1, cone.DepthStructural); err != nil {
		return err
	}
	if got := hzPragmas(peer.sent()[0], "horizon-mergebase"); len(got) != 1 || got[0][0] != x.c3 || got[0][1] != x.feat1 {
		return expectf("the deepen asked %q, want the base of c3 and feat1", got)
	}
	return hzElided(r, nil)
}

// hzDeepenFailed: a deepen whose synchronization fails leaves the store as it
// was, the horizon and the seed for C both, as zit's does.
func hzDeepenFailed(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	held := mustHeld(t, r)
	if _, err := sync.Deepen(context.Background(), r, hzFailingPeer{}, x.c2, sync.SyncOpts{}); err == nil {
		return expectf("a deepen through a failing peer succeeded")
	}
	if err := hzStoredHorizon(r, x.c3, cone.DepthStructural); err != nil {
		return err
	}
	if got := mustHeld(t, r); string(got) != string(held) {
		return expectf("the failed deepen left seed %q, want the seed for c3 back", got)
	}
	return x.s.derivesDense(t, r)
}

type hzFailingPeer struct{}

func (hzFailingPeer) Exchange(context.Context, *xfer.Message) (*xfer.Message, error) {
	return nil, errors.New("peer unreachable")
}

// hzStoredHorizon is nil when r's store cone carries a horizon at c, of the
// descendants rule, at depth d.
func hzStoredHorizon(r *repo.Repo, c string, d cone.HorizonDepth) error {
	sc, err := content.StoreCone(r.DB())
	if err != nil {
		return err
	}
	if sc == nil {
		return expectf("the store is dense")
	}
	h, ok := sc.Horizon()
	if !ok || h.Hash() != c || h.Rule() != cone.RuleDescendants || h.Depth() != d {
		return expectf("stored horizon %v %s %v %v, want %.10s descendants %v", ok, h.Hash(), h.Rule(), h.Depth(), c, d)
	}
	return nil
}

// hzHeldNames is every artifact r holds, by name.
func hzHeldNames(r *repo.Repo) (map[string]bool, error) {
	rows, err := r.DB().Query("SELECT uuid FROM blob WHERE size>=0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}
