package conformance

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gofossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/db"

	"github.com/danmestas/go-libfossil/internal/bisect"
	"github.com/danmestas/go-libfossil/internal/branch"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The merge-base query and deepen (draft-fossil-horizon-00 Appendix A step
// 6: §5.5, §5.7, §5.8, §5.9, §6.2.4) over hzStruct: go-libfossil's handler
// is the origin, its structural clone the client.

func horizonDeepenCases() []Case {
	return []Case{horizonMergeBaseQuery, horizonMergeBaseBound, horizonDeepen, horizonDeepenPoint, horizonDeepenFailed, horizonDeepenRefused,
		horizonMergeCrossing, horizonMergeDeepen, horizonMergeMustAsk, horizonMergeListForm, horizonPushCheck,
		horizonSeedVerify, horizonBisectCrossing}
}

var (
	horizonMergeBaseQuery = Case{ID: "horizon.6.2.4.merge-base", Req: "HZ-6.2-7"}
	horizonMergeBaseBound = Case{ID: "horizon.8.merge-base-bound", Req: "HZ-8-1"}
	horizonDeepen         = Case{ID: "horizon.5.8.deepen", Req: "HZ-5.8-1"}
	horizonDeepenPoint    = Case{ID: "horizon.5.8.deepen-point", Req: "HZ-5.8-1"}
	horizonDeepenFailed   = Case{ID: "horizon.5.8.deepen-failed", Req: "HZ-5.8-1"}
	horizonDeepenRefused  = Case{ID: "horizon.5.8.deepen-refused", Req: "HZ-5.8-2"}
	horizonMergeCrossing  = Case{ID: "horizon.5.5.merge-crossing", Req: "HZ-5.5-1"}
	horizonMergeDeepen    = Case{ID: "horizon.5.5.merge-deepen", Req: "HZ-5.5-2"}
	horizonMergeMustAsk   = Case{ID: "horizon.5.5.merge-must-ask", Req: "HZ-5.5-3"}
	horizonMergeListForm  = Case{ID: "horizon.6.2.4.merge-pending", Req: "HZ-6.2-16"}
	horizonPushCheck      = Case{ID: "horizon.5.7.push-check", Req: "HZ-5.7-2"}
	horizonSeedVerify     = Case{ID: "horizon.5.9.verify-seed", Req: "HZ-5.9-1"}
	horizonBisectCrossing = Case{ID: "horizon.5.5.bisect-crossing", Req: "HZ-5.5-1"}
)

func TestHorizonDeepen(t *testing.T) {
	check(t, horizonMergeBaseQuery, func(t *testing.T) error { return hzMergeBaseQuery(t) })
	check(t, horizonMergeBaseBound, func(t *testing.T) error { return hzMergeBaseBound(t) })
	check(t, horizonDeepen, func(t *testing.T) error { return hzDeepen(t) })
	check(t, horizonDeepenPoint, func(t *testing.T) error { return hzDeepenPoint(t) })
	check(t, horizonDeepenFailed, func(t *testing.T) error { return hzDeepenFailed(t) })
	check(t, horizonDeepenRefused, func(t *testing.T) error { return hzDeepenRefused(t) })
	check(t, horizonMergeCrossing, func(t *testing.T) error { return hzMergeCrossing(t) })
	check(t, horizonMergeDeepen, func(t *testing.T) error { return hzMergeDeepen(t) })
	check(t, horizonMergeMustAsk, func(t *testing.T) error { return hzMergeMustAsk(t) })
	check(t, horizonMergeListForm, func(t *testing.T) error { return hzMergeListForm(t) })
	check(t, horizonPushCheck, func(t *testing.T) error { return hzPushCheck(t) })
	check(t, horizonSeedVerify, func(t *testing.T) error { return hzSeedVerify(t) })
	check(t, horizonBisectCrossing, func(t *testing.T) error { return hzBisectCrossing(t) })
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
		res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true,
			MergeBaseOf: sync.MergeBaseQuery{Primary: q[0], Secondaries: []string{q[1]}}})
		if err != nil || !res.MergeBaseAnswered || res.MergeBase != q[2] {
			return expectf("base of %.10s and %.10s: %v answered=%v %q, want %.10s",
				q[0], q[1], err, res.MergeBaseAnswered, res.MergeBase, q[2])
		}
	}
	root := hzCommit(t, x.s.f, "unrelated", []libfossil.FslID{0}, nil)
	res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true,
		MergeBaseOf: sync.MergeBaseQuery{Primary: x.c4, Secondaries: []string{hzUUID(t, x.s.f.r, root)}}})
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
	_, want, err := seed.Mint(x.s.f.r.DB(), x.s.c2, cone.RuleDescendants, k, seedLoginGo)
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
	if got := hzPragmas(peer.sent()[0], "horizon-mergebase"); len(got) != 1 || !slices.Equal(got[0], []string{x.feat1, x.c3}) {
		return expectf("the deepen asked %q, want the base of feat1 over c3", got)
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
// hzDeepenRefused: a deepen toward a check-in that shares no history with C,
// where the origin answers -, and toward one the origin does not hold,
// where it answers nothing, has no common ancestor to move C to. It is
// refused naming the remedy, a fresh clone with an earlier horizon, and C,
// the seed and the store stay as they were: C never moves to a check-in
// that is not its ancestor (§5.8).
func hzDeepenRefused(t *testing.T) error {
	x := newHZStruct(t)
	unrelated := hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "unrelated", []libfossil.FslID{0}, nil))
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	held := mustHeld(t, r)
	for _, target := range []string{unrelated, strings.Repeat("ab", 20)} {
		_, err := sync.Deepen(context.Background(), r, x.peer(), target, sync.SyncOpts{})
		if !errors.Is(err, sync.ErrNoDeepenPoint) {
			return expectf("deepen to %.10s: %v, want ErrNoDeepenPoint", target, err)
		}
		if !strings.Contains(err.Error(), "clone") || !strings.Contains(err.Error(), "earlier horizon") {
			return expectf("the refusal %q does not name the remedy, a clone at an earlier horizon", err)
		}
		if err := hzStoredHorizon(r, x.c3, cone.DepthStructural); err != nil {
			return err
		}
		if got := mustHeld(t, r); string(got) != string(held) {
			return expectf("the refused deepen changed the seed to %q", got)
		}
	}
	return x.s.derivesDense(t, r)
}

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

// hzMergeFixture is hzStruct's origin served over HTTP, with feat3, a child
// of feat1 committed after everything else; w and z, which merge feat3 into
// c4 and into feat2; and y, which merges feat1 into c4. Its client is a
// structural clone at C = c3 with a checkout at feat2.
//
// fossil's merge base (pivot.c), the most recent common ancestor, of w and z
// is feat3, below the horizon: the store holds w and z but not their base.
// That of y and feat2 is c3, which the store holds, but feat1, a common
// ancestor too, is below it: the store's walk crosses the horizon on the way.
// d1, d2 and d3 are children of c4, committed last, whose base the store's
// walk finds at c4 without crossing; their ancestry crosses all the same.
type hzMergeFixture struct {
	x           *hzStruct
	feat3, w, z string
	y           string
	d1, d2, d3  string
	origin      *hzRecorder
	url         string
	r           *gofossil.Repo
	co          *gofossil.Checkout
}

func newHZMergeFixture(t *testing.T) *hzMergeFixture {
	t.Helper()
	return newHZMergeFixtureAt(t, false)
}

// newHZMergeFixtureAt is newHZMergeFixture, its clone's horizon at c1, the
// root check-in, when root is set, so that no ancestry crosses it.
func newHZMergeFixtureAt(t *testing.T, root bool) *hzMergeFixture {
	t.Helper()
	x := newHZStruct(t)
	f := &hzMergeFixture{x: x, origin: &hzRecorder{r: x.s.f.r}}
	f.y = hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "y", []libfossil.FslID{x.s.c4, x.s.feat1}, nil))
	feat3 := hzCommit(t, x.s.f, "feat3", []libfossil.FslID{x.s.feat1}, nil)
	f.feat3 = hzUUID(t, x.s.f.r, feat3)
	f.w = hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "w", []libfossil.FslID{x.s.c4, feat3}, nil))
	f.z = hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "z", []libfossil.FslID{x.s.feat2, feat3}, nil))
	f.d1 = hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "d1", []libfossil.FslID{x.s.c4}, nil))
	f.d2 = hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "d2", []libfossil.FslID{x.s.c4}, nil))
	f.d3 = hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "d3", []libfossil.FslID{x.s.c4}, nil))
	server := httptest.NewServer(f.origin)
	t.Cleanup(server.Close)
	f.url = server.URL
	c := x.c3
	if root {
		c = x.c1
	}
	hc, err := gofossil.NewHorizonCone(nil, nil, gofossil.Horizon{Checkin: c, Structural: true})
	if err != nil {
		t.Fatal(err)
	}
	r, res, err := gofossil.Clone(context.Background(), filepath.Join(t.TempDir(), "structural.fossil"),
		gofossil.NewHTTPTransport(f.url), gofossil.CloneOpts{StoreCone: hc, PullPhantoms: true})
	if err != nil || res.HorizonFallback {
		t.Fatalf("structural clone: %v %+v", err, res)
	}
	t.Cleanup(func() { r.Close() })
	f.r = r
	co, err := r.CreateCheckout(t.TempDir(), gofossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	if err := co.Extract(f.rid(t, x.feat2), gofossil.ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	f.co = co
	f.origin.take()
	return f
}

func (f *hzMergeFixture) rid(t *testing.T, uuid string) int64 {
	t.Helper()
	rid, err := f.r.ResolveVersion(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

// hzMergeCrossing: a merge whose base lies below the structural horizon is
// detected before anything changes, and the diagnostic names the horizon and
// the remedy; so is a merge whose base the store holds when its walk to it
// crossed the horizon, where an older-looking ancestor below it could be the
// base. Neither names a base: the store cannot know it.
func hzMergeCrossing(t *testing.T) error {
	f := newHZMergeFixture(t)
	for _, m := range [][2]string{{f.z, f.w}, {f.x.feat2, f.y}} {
		if err := f.co.Extract(f.rid(t, m[0]), gofossil.ExtractOpts{}); err != nil {
			return err
		}
		_, err := f.co.Merge(gofossil.CheckoutMergeOpts{Version: f.rid(t, m[1])})
		if !errors.Is(err, gofossil.ErrBelowHorizon) {
			return expectf("merge of %.10s across the horizon: %v, want ErrBelowHorizon", m[1], err)
		}
		for _, want := range []string{"below the structural horizon at " + f.x.c3, "deepen"} {
			if !strings.Contains(err.Error(), want) {
				return expectf("diagnostic %q lacks %q", err, want)
			}
		}
		if v, _, err := f.co.Version(); err != nil || v != f.rid(t, m[0]) {
			return expectf("the refused merge moved the checkout to %d (%v)", v, err)
		}
		changed, err := f.co.HasChanges()
		if err != nil || changed {
			return expectf("the refused merge left changes %v (%v)", changed, err)
		}
	}
	return nil
}

// hzMergeDeepen: given a peer, the merge asks the origin for the base,
// deepens to a common ancestor of C and the base, and merges over the real
// base it then holds. Where the store holds the base the origin names, it
// merges over it without deepening.
func hzMergeDeepen(t *testing.T) error {
	f := newHZMergeFixture(t)
	peer := gofossil.RequiredContentOpts{Peer: gofossil.NewHTTPTransport(f.url)}
	res, err := f.co.MergeContext(context.Background(), gofossil.CheckoutMergeOpts{Version: f.rid(t, f.y)}, peer)
	if err != nil {
		return expectf("merge of y with a peer: %v", err)
	}
	if res.Ancestor != f.rid(t, f.x.c3) {
		return expectf("merged y against rid %d, want c3's", res.Ancestor)
	}
	if err := hzAskedMergeBase(f, f.y, f.x.feat2); err != nil {
		return err
	}
	if err := hzStoredHorizon(f.r.Inner(), f.x.c3, cone.DepthStructural); err != nil {
		return expectf("merging over a held base deepened: %v", err)
	}
	f = newHZMergeFixture(t)
	peer = gofossil.RequiredContentOpts{Peer: gofossil.NewHTTPTransport(f.url)}
	if err := f.co.Extract(f.rid(t, f.z), gofossil.ExtractOpts{}); err != nil {
		return err
	}
	res, err = f.co.MergeContext(context.Background(), gofossil.CheckoutMergeOpts{Version: f.rid(t, f.w)}, peer)
	if err != nil {
		return expectf("merge of w with a peer: %v", err)
	}
	if res.Ancestor != f.rid(t, f.feat3) {
		return expectf("merged w against rid %d, want feat3's", res.Ancestor)
	}
	if err := hzAskedMergeBase(f, f.w, f.z); err != nil {
		return err
	}
	sc, err := f.r.StoreCone()
	if err != nil {
		return err
	}
	if h, ok := sc.Horizon(); !ok || h.Checkin != f.x.c1 || !h.Structural {
		return expectf("store horizon after the merge %+v %v, want c1 structural", h, ok)
	}
	return nil
}

// hzAskedMergeBase is nil when the origin was asked the base of q first. A
// deepen may ask another, for its deepen point.
// hzAskedMergeBase checks that the first merge-base query the origin saw
// since the last take is primary over secondaries, in that order (§6.2.4).
func hzAskedMergeBase(f *hzMergeFixture, primary string, secondaries ...string) error {
	var asked [][]string
	for _, e := range f.origin.take() {
		asked = append(asked, hzPragmas(e.request, "horizon-mergebase")...)
	}
	want := append([]string{primary}, secondaries...)
	if len(asked) == 0 || !slices.Equal(asked[0], want) {
		return expectf("the merge asked the origin %q, first %q", asked, want)
	}
	return nil
}

// hzMergeMustAsk: §5.5 has the clone ask the origin whenever either side's
// ancestry crosses the horizon, even when its own walk finds a common
// ancestor. Merging d2 into a checkout of d1, the walk finds c4 without
// reaching below C, but c4's ancestry crosses at c3: MergeContext asks the
// origin for the pivot of d2 over d1 and merges over its answer, c4, which
// the store holds, so nothing deepens. Without a peer there is no one to ask,
// so Merge refuses, naming the horizon and the remedy, whenever either
// side's ancestry crosses: when its own walk crosses (y into feat2) and when
// the walk finds c4 but the ancestry below it crosses (d2 into d1). It
// merges locally only when no ancestry crosses, as at a horizon at the root.
func hzMergeMustAsk(t *testing.T) error {
	f := newHZMergeFixture(t)
	peer := gofossil.RequiredContentOpts{Peer: gofossil.NewHTTPTransport(f.url)}
	if err := f.co.Extract(f.rid(t, f.d1), gofossil.ExtractOpts{}); err != nil {
		return err
	}
	res, err := f.co.MergeContext(context.Background(), gofossil.CheckoutMergeOpts{Version: f.rid(t, f.d2)}, peer)
	if err != nil {
		return expectf("merge of d2 with a peer: %v", err)
	}
	if res.Ancestor != f.rid(t, f.x.c4) {
		return expectf("merged d2 against rid %d, want c4's", res.Ancestor)
	}
	if err := hzAskedMergeBase(f, f.d2, f.d1); err != nil {
		return err
	}
	if err := hzStoredHorizon(f.r.Inner(), f.x.c3, cone.DepthStructural); err != nil {
		return expectf("merging over a held base deepened: %v", err)
	}
	f = newHZMergeFixture(t)
	for _, m := range [][2]string{{f.x.feat2, f.y}, {f.d1, f.d2}} {
		if err := f.co.Extract(f.rid(t, m[0]), gofossil.ExtractOpts{}); err != nil {
			return err
		}
		_, err := f.co.Merge(gofossil.CheckoutMergeOpts{Version: f.rid(t, m[1])})
		if !errors.Is(err, gofossil.ErrBelowHorizon) {
			return expectf("merge of %.10s into %.10s without a peer: %v, want ErrBelowHorizon", m[1], m[0], err)
		}
		for _, want := range []string{"structural horizon at " + f.x.c3, "MergeContext"} {
			if !strings.Contains(err.Error(), want) {
				return expectf("diagnostic %q lacks %q", err, want)
			}
		}
		if changed, err := f.co.HasChanges(); err != nil || changed {
			return expectf("the refused merge left changes %v (%v)", changed, err)
		}
	}
	if _, _, err := f.r.Merge("feat", "trunk", "merge feat", "u"); !errors.Is(err, gofossil.ErrBelowHorizon) {
		return expectf("branch merge across the horizon without a peer: %v, want ErrBelowHorizon", err)
	}
	if err := hzNoMergeBaseAsked(f); err != nil {
		return err
	}
	f = newHZMergeFixtureAt(t, true)
	if err := f.co.Extract(f.rid(t, f.d1), gofossil.ExtractOpts{}); err != nil {
		return err
	}
	res, err = f.co.Merge(gofossil.CheckoutMergeOpts{Version: f.rid(t, f.d2)})
	if err != nil || res.Ancestor != f.rid(t, f.x.c4) {
		return expectf("merge of d2 at a horizon nothing crosses: %v ancestor %d, want c4's", err, res.Ancestor)
	}
	return hzNoMergeBaseAsked(f)
}

// hzNoMergeBaseAsked fails when the origin was sent a merge-base query
// since the last take.
func hzNoMergeBaseAsked(f *hzMergeFixture) error {
	for _, e := range f.origin.take() {
		if got := hzPragmas(e.request, "horizon-mergebase"); len(got) != 0 {
			return expectf("a merge without a peer asked %q", got)
		}
	}
	return nil
}

// hzMergeListForm: §6.2.4's secondaries are the checkout and each plain
// merge pending in it. With d2 already merged into a checkout of d1, a merge
// of d3 asks the pivot of d3 over d1 and d2. A query carries at most two
// secondaries, so with d2 and d3 both pending a merge that must ask cannot,
// and MergeContext refuses before anything changes, naming why.
func hzMergeListForm(t *testing.T) error {
	f := newHZMergeFixture(t)
	peer := gofossil.RequiredContentOpts{Peer: gofossil.NewHTTPTransport(f.url)}
	if err := f.co.Extract(f.rid(t, f.d1), gofossil.ExtractOpts{}); err != nil {
		return err
	}
	if _, err := f.co.MergeContext(context.Background(), gofossil.CheckoutMergeOpts{Version: f.rid(t, f.d2)}, peer); err != nil {
		return err
	}
	f.origin.take()
	res, err := f.co.MergeContext(context.Background(), gofossil.CheckoutMergeOpts{Version: f.rid(t, f.d3)}, peer)
	if err != nil {
		return expectf("merge of d3 with d2 pending: %v", err)
	}
	if res.Ancestor != f.rid(t, f.x.c4) {
		return expectf("merged d3 against rid %d, want c4's", res.Ancestor)
	}
	if err := hzAskedMergeBase(f, f.d3, f.d1, f.d2); err != nil {
		return err
	}
	_, err = f.co.MergeContext(context.Background(), gofossil.CheckoutMergeOpts{Version: f.rid(t, f.y)}, peer)
	if err == nil || !strings.Contains(err.Error(), "§6.2.4") {
		return expectf("merge of y with d2 and d3 pending: %v, want a refusal citing §6.2.4", err)
	}
	for _, e := range f.origin.take() {
		if got := hzPragmas(e.request, "horizon-mergebase"); len(got) != 0 {
			return expectf("the refused merge asked %q", got)
		}
	}
	return nil
}

// hzPushCheck: the origin moves c4 to branch moved after a structural clone
// took its seed. The clone, re-seeded by nothing yet, creates a branch from
// c4 cancelling sym-trunk; the origin refuses the push, naming the stale seed
// and moved, and stores nothing. After a pull re-seeds the clone, the clone
// writes the control artifact cancelling sym-moved on its check-in and
// pushes again, and the origin admits both and derives the check-in on the
// new branch alone.
func hzPushCheck(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	x.s.tag(t, x.s.c4, "branch", tag.TagPropagating, "moved")
	x.s.tag(t, x.s.c4, "sym-moved", tag.TagPropagating, "")
	x.s.tag(t, x.s.c4, "sym-trunk", tag.TagCancel, "")
	c4, err := hzRID(r, x.c4)
	if err != nil {
		return err
	}
	when := x.s.f.when.Add(24 * time.Hour)
	nb, _, err := branch.Create(r, branch.CreateOpts{Name: "nb", Parent: c4, User: "u", Time: when})
	if err != nil {
		return err
	}
	nbUUID := hzUUID(t, r, nb)
	res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Push: true})
	if !hzRefusedStale(err, res, "'moved'") || !hzRefusedStale(err, res, "'trunk'") {
		return expectf("push of a stale cancellation: %v %+v", err, res)
	}
	if names, err := hzHeldNames(x.s.f.r); err != nil || names[nbUUID] {
		return expectf("the origin holds the refused check-in (%v)", err)
	}
	if res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true}); err != nil || res.HorizonFallback {
		return expectf("re-sync: %v %+v", err, res)
	}
	if _, err := tag.AddTag(r, tag.TagOpts{TargetRID: nb, TagName: "sym-moved", TagType: tag.TagCancel,
		User: "u", Time: when.Add(time.Hour)}); err != nil {
		return err
	}
	res, err = sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Push: true})
	if err != nil || len(res.Errors) != 0 {
		return expectf("push with the correction: %v %q", err, res.Errors)
	}
	got, err := hzTagState(x.s.f.r.DB(), nbUUID)
	if err != nil || !slices.Contains(got, "branch/2=nb") || slices.ContainsFunc(got, func(s string) bool {
		return strings.HasPrefix(s, "sym-moved") || strings.HasPrefix(s, "sym-trunk")
	}) {
		return expectf("the origin derives the corrected check-in as %v (%v)", got, err)
	}
	return nil
}

// hzSeedVerify: verification by deepening (§5.9) holds the real tag artifacts
// and primary-parent chains below C, derives every entry's tag state from
// them and compares it with what the seed asserted. An honest seed verifies;
// one whose hue was altered after delivery is reported at its entry. The
// store is left a content-horizon store holding no seed.
func hzSeedVerify(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	report, err := sync.VerifySeed(context.Background(), r, x.peer(), sync.SyncOpts{})
	if err != nil || report.Checked == 0 || len(report.Mismatches) != 0 {
		return expectf("an honest seed: %v %+v", err, report)
	}
	if err := hzStoredHorizon(r, x.c3, cone.DepthContent); err != nil {
		return err
	}
	if held, err := seed.Held(r.DB()); err != nil || held != nil {
		return expectf("a verified store still holds a seed (%v)", err)
	}

	x = newHZStruct(t)
	r, _, err = x.clone(t, x.peer())
	if err != nil {
		return err
	}
	d, err := deck.Parse(mustHeld(t, r))
	if err != nil {
		return err
	}
	for i := range d.T {
		if d.T[i].Name == "hue" {
			d.T[i].Value = "green"
		}
	}
	forged, err := d.Marshal()
	if err != nil {
		return err
	}
	if err := r.WithTx(func(tx *db.Tx) error { return seed.Hold(tx, hash.SHA3(forged), forged, nil) }); err != nil {
		return err
	}
	report, err = sync.VerifySeed(context.Background(), r, x.peer(), sync.SyncOpts{})
	if err != nil {
		return err
	}
	if len(report.Mismatches) == 0 {
		return expectf("an altered seed verified: %+v", report)
	}
	for _, m := range report.Mismatches {
		if m.Tag != "hue" || m.Seeded != "*green" || m.Derived != "*red" {
			return expectf("mismatch %+v, want hue seeded *green, derived *red", m)
		}
	}
	return nil
}

// hzBisectCrossing: a bisection whose good end lies below the structural
// horizon is refused before it begins, naming the horizon and the deepen;
// one within K runs.
func hzBisectCrossing(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	if _, err := r.DB().Exec("CREATE TABLE IF NOT EXISTS vvar(name TEXT PRIMARY KEY, value CLOB) WITHOUT ROWID"); err != nil {
		return err
	}
	rid := func(uuid string) libfossil.FslID {
		id, err := hzRID(r, uuid)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	s := bisect.NewSession(r.DB().SqlDB())
	if err := s.MarkGood(rid(x.c2)); err != nil {
		return err
	}
	if err := s.MarkBad(rid(x.c4)); err != nil {
		return err
	}
	_, err = s.Next()
	if !errors.Is(err, content.ErrBelowHorizon) || !strings.Contains(err.Error(), "below the structural horizon at "+x.c3) ||
		!strings.Contains(err.Error(), "repo cone deepen "+x.c2) {
		return expectf("bisect from below the horizon: %v, want a diagnostic naming the horizon and the deepen", err)
	}
	if err := s.MarkGood(rid(x.c3)); err != nil {
		return err
	}
	if _, err := s.Next(); err != nil && !errors.Is(err, bisect.ErrBisectComplete) {
		return expectf("bisect within K: %v", err)
	}
	return nil
}
