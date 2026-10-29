package conformance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// A horizon store serving horizon peers (draft-fossil-horizon-00 §6.2.3,
// §6.3, §6.4): the origin is itself a horizon clone. It serves a request
// whose held set lies inside its own, refuses one that does not, and, at a
// structural horizon, answers a structural request at the content level
// instead of minting a seed, naming the history interface a dense origin
// keeps so the requester's fallback can say where the history below lies.

var (
	horizonPeerRefusePull = Case{ID: "horizon.6.4.refuse-pull", Req: "HZ-6.4-1"}
	horizonPeerNoSeed     = Case{ID: "horizon.6.2.3.seeded-store-no-seed", Req: "HZ-6.2-6"}
	horizonPeerFallback   = Case{ID: "horizon.6.3.structural-peer-fallback", Req: "HZ-6.3-2"}
)

func horizonPeerCases() []Case {
	return []Case{horizonPeerRefusePull, horizonPeerNoSeed, horizonPeerFallback}
}

func TestHorizonPeer(t *testing.T) {
	check(t, horizonPeerRefusePull, func(t *testing.T) error { return hzPeerRefusePull(t) })
	check(t, horizonPeerNoSeed, func(t *testing.T) error { return hzPeerNoSeed(t) })
	check(t, horizonPeerFallback, func(t *testing.T) error { return hzPeerFallback(t) })
}

// hzPeerRefusal is the one error card a horizon store answers a horizon
// pull with when the requested held set is not a subset of its own.
const hzPeerRefusal = "Horizon store does not hold the requested horizon; pull from an origin that holds it."

// hzPeerHistory is the history interface the tests' horizon stores name.
const hzPeerHistory = "https://example.org/history?repo=dense"

// hzPeerPull is a horizon pull request at c under rule for store's project.
func hzPeerPull(t *testing.T, store *repo.Repo, c, rule string) *xfer.Message {
	t.Helper()
	code, err := store.Config("project-code")
	if err != nil {
		t.Fatal(err)
	}
	return &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "horizon", Values: []string{c, rule}},
		&xfer.PullCard{ServerCode: "x", ProjectCode: code},
	}}
}

// hzPeerOpen lets nobody read and clone store.
func hzPeerOpen(t *testing.T, store *repo.Repo) {
	t.Helper()
	if _, err := store.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
}

// hzBlobCount is how many blob rows r holds, phantoms included.
func hzBlobCount(t *testing.T, r *repo.Repo) int {
	t.Helper()
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A content-horizon store at C serves a horizon pull at C and refuses one at
// an earlier C, whose held set it does not hold, with one error card and
// nothing else. A go-libfossil client at that earlier C, pulling or
// syncing, takes the refusal as fatal and stores nothing.
func hzPeerRefusePull(t *testing.T) error {
	g := newHZSyncGraph(t)
	_, store, _, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		return err
	}
	hzPeerOpen(t, store)
	server := &scSyncPeer{r: store}
	held, err := server.Exchange(context.Background(), hzPeerPull(t, store, hzUUID(t, g.f.r, g.c3), "descendants"))
	if err != nil {
		return err
	}
	if got := hzPragmas(held, "horizon-served"); len(got) != 1 {
		return expectf("a pull at the store's own C: horizon-served %q, want one", got)
	}
	for _, card := range held.Cards {
		if c, ok := card.(*xfer.ErrorCard); ok {
			return expectf("a pull at the store's own C was refused: %s", c.Message)
		}
	}
	earlier := hzUUID(t, g.f.r, g.c2)
	refused, err := server.Exchange(context.Background(), hzPeerPull(t, store, earlier, "descendants"))
	if err != nil {
		return err
	}
	if err := scSyncRefused(refused, hzPeerRefusal); err != nil {
		return expectf("a pull at an earlier C: %v", err)
	}
	if len(refused.Cards) != 1 {
		return expectf("a pull at an earlier C was answered with %d cards, want the error alone", len(refused.Cards))
	}
	_, relay, _, err := hzServeClone(t, g, hzStoreCone(t, earlier, cone.RuleDescendants, cone.DepthContent),
		sync.HandleOpts{})
	if err != nil {
		return err
	}
	hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	before := hzBlobCount(t, relay)
	for _, opts := range []sync.SyncOpts{{Pull: true}, {Pull: true, Push: true}} {
		_, err := sync.Sync(context.Background(), relay, server, opts)
		if err == nil || !strings.Contains(err.Error(), hzPeerRefusal) {
			return expectf("a client at an earlier C (push %v): %v, want the refusal as fatal", opts.Push, err)
		}
	}
	if after := hzBlobCount(t, relay); after != before {
		return expectf("the refused client went from %d blob rows to %d", before, after)
	}
	return nil
}

// newHZSeededPeer makes a structural clone of x's dense origin that names the
// origin's history interface and lets nobody read and clone it.
func newHZSeededPeer(t *testing.T, x *hzStruct) (*repo.Repo, error) {
	t.Helper()
	store, result, err := x.clone(t, x.peer())
	if err != nil {
		return nil, err
	}
	if result.HorizonFallback || result.StructuralServed == 0 {
		return nil, expectf("fixture: the structural store fell back")
	}
	if err := store.SetConfig(repo.SettingHorizonHistoryURL, hzPeerHistory); err != nil {
		return nil, err
	}
	hzPeerOpen(t, store)
	return store, nil
}

// hzPeerRemedy fails unless remedy names C and the dense origin's history.
func hzPeerRemedy(remedy, c string) error {
	if !strings.Contains(remedy, c) || !strings.Contains(remedy, hzPeerHistory) {
		return expectf("remedy %q names neither C %s nor the dense origin %s", remedy, c, hzPeerHistory)
	}
	return nil
}

// A store at a structural horizon does not mint a seed: it answers a
// structural clone at the same C as content, with no seed and no boundary
// label, and names its history interface. The clone falls back to the
// content horizon and says where the manifests below C are.
func hzPeerNoSeed(t *testing.T) error {
	x := newHZStruct(t)
	store, err := newHZSeededPeer(t, x)
	if err != nil {
		return err
	}
	peer := &scSyncPeer{r: store}
	r, result, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "relay.fossil"),
		peer, sync.CloneOpts{StoreCone: x.cone(t)})
	if err != nil {
		return expectf("a structural clone of a structural store: %v", err)
	}
	defer r.Close()
	if result.StructuralServed != 0 || result.HorizonServed == 0 || !result.HorizonFallback {
		return expectf("served structural %d, horizon %d, fell back %v; want content and a fallback",
			result.StructuralServed, result.HorizonServed, result.HorizonFallback)
	}
	for i, reply := range peer.replies {
		if got := hzPragmas(reply, "horizon-served"); len(got) != 1 || strings.Join(got[0], " ") != "content "+hzPeerHistory {
			return expectf("reply %d carries horizon-served %q, want content %s", i, got, hzPeerHistory)
		}
		if got := len(hzPragmas(reply, "horizon-seed")) + len(hzPragmas(reply, "horizon-below")); got != 0 {
			return expectf("reply %d carries %d seed or boundary pragmas", i, got)
		}
	}
	if d, err := hzStructDepth(r); err != nil || d != cone.DepthContent {
		return expectf("stored depth %v (%v), want content", d, err)
	}
	if held, _ := seed.Held(r.DB()); held != nil {
		return expectf("the relay holds a seed")
	}
	return hzPeerRemedy(result.HorizonRemedy, x.c3)
}

// A structural clone that pulls from a structural peer at the same C falls
// back with a remedy naming the dense origin, and pulling from that origin
// then fills the manifests below C.
func hzPeerFallback(t *testing.T) error {
	x := newHZStruct(t)
	store, err := newHZSeededPeer(t, x)
	if err != nil {
		return err
	}
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	result, err := sync.Sync(context.Background(), r, &scSyncPeer{r: store}, sync.SyncOpts{Pull: true})
	if err != nil {
		return expectf("a structural pull from a structural peer: %v", err)
	}
	if !result.HorizonFallback || result.StructuralServed != 0 {
		return expectf("fell back %v, served structural %d; want a content answer and a fallback",
			result.HorizonFallback, result.StructuralServed)
	}
	if err := hzPeerRemedy(result.HorizonRemedy, x.c3); err != nil {
		return err
	}
	if held, _ := seed.Held(r.DB()); held != nil {
		return expectf("the seed survived the fallback")
	}
	if err := hzHeld(r, x.below(), false); err != nil {
		return expectf("the structural peer sent what it does not hold: %v", err)
	}
	if _, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	return hzHeld(r, x.below(), true)
}
