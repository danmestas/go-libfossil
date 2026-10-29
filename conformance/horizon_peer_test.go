package conformance

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/manifest"
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
	horizonPeerRefusePush = Case{ID: "horizon.6.4.refuse-keeps-push", Req: "HZ-6.4-1"}
	horizonPeerLaterC     = Case{ID: "horizon.6.4.later-c-served", Req: "HZ-6.4-1"}
	horizonPeerNoSeed     = Case{ID: "horizon.6.2.3.seeded-store-no-seed", Req: "HZ-6.2-6"}
	horizonPeerFallback   = Case{ID: "horizon.6.3.structural-peer-fallback", Req: "HZ-6.3-2"}
)

func horizonPeerCases() []Case {
	return []Case{horizonPeerRefusePull, horizonPeerRefusePush, horizonPeerLaterC, horizonPeerNoSeed,
		horizonPeerFallback}
}

func TestHorizonPeer(t *testing.T) {
	check(t, horizonPeerRefusePull, func(t *testing.T) error { return hzPeerRefusePull(t) })
	check(t, horizonPeerRefusePush, func(t *testing.T) error { return hzPeerRefusePush(t) })
	check(t, horizonPeerLaterC, func(t *testing.T) error { return hzPeerLaterC(t) })
	check(t, horizonPeerNoSeed, func(t *testing.T) error { return hzPeerNoSeed(t) })
	check(t, horizonPeerFallback, func(t *testing.T) error { return hzPeerFallback(t) })
}

// hzPeerRefusalPrefix begins the one error card a horizon store answers a
// horizon pull with when the requested held set is not a subset of its own;
// a client matches the prefix, not the human text after it.
const hzPeerRefusalPrefix = "horizon-not-held:"

// hzPeerRefusal is go-libfossil's whole refusal text.
const hzPeerRefusal = hzPeerRefusalPrefix + " this store does not hold the requested horizon; pull from an origin that holds it"

// hzPeerFallbackPrefix begins the diagnostic a store that fell back reports
// among its sync errors or clone messages.
const hzPeerFallbackPrefix = "horizon-fallback:"

// hzPeerHistory is the history interface the tests' horizon stores name.
const hzPeerHistory = "https://example.org/history?repo=dense"

// hzPeerPull is a horizon pull request at c under rule for store's project,
// carrying extra cards ahead of the pull card.
func hzPeerPull(t *testing.T, store *repo.Repo, c, rule string, extra ...xfer.Card) *xfer.Message {
	t.Helper()
	code, err := store.Config("project-code")
	if err != nil {
		t.Fatal(err)
	}
	cards := []xfer.Card{&xfer.PragmaCard{Name: "horizon", Values: []string{c, rule}}}
	cards = append(cards, extra...)
	return &xfer.Message{Cards: append(cards, &xfer.PullCard{ServerCode: "x", ProjectCode: code})}
}

// hzPeerOpen lets nobody read, write and clone store.
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

// hzPeerServed fails unless reply served the horizon and refused nothing.
func hzPeerServed(reply *xfer.Message) error {
	if got := hzPragmas(reply, "horizon-served"); len(got) != 1 {
		return expectf("horizon-served %q, want one", got)
	}
	for _, card := range reply.Cards {
		if c, ok := card.(*xfer.ErrorCard); ok {
			return expectf("refused: %s", c.Message)
		}
	}
	return nil
}

// hzPeerRefused fails unless reply refuses the pull half: its last card is
// the one horizon-not-held error, and it carries no content, no
// announcement and no pragma serving the horizon. The push half's cards
// may precede the error, and only the card types a refusal allows: gimmes,
// errors, messages and session-level pragmas.
func hzPeerRefused(reply *xfer.Message) error {
	n := len(reply.Cards)
	if n == 0 {
		return expectf("an empty reply")
	}
	if last, ok := reply.Cards[n-1].(*xfer.ErrorCard); !ok || last.Message != hzPeerRefusal {
		return expectf("the last card is %T %v, want the error %q", reply.Cards[n-1], reply.Cards[n-1], hzPeerRefusal)
	}
	if sent := scSyncPayloads(reply); len(sent) != 0 {
		return expectf("the refusal carries content %v", sent)
	}
	for _, card := range reply.Cards {
		switch c := card.(type) {
		case *xfer.GimmeCard, *xfer.UVGimmeCard, *xfer.XGimmeCard, *xfer.ErrorCard, *xfer.MessageCard:
		case *xfer.PragmaCard:
			if !hzPeerRefusalPragmas[c.Name] {
				return expectf("the refusal carries pragma %s", c.Name)
			}
		default:
			return expectf("the refusal carries a %T card", c)
		}
	}
	return nil
}

// hzPeerRefusalPragmas are the session-level pragmas a refusal may keep.
var hzPeerRefusalPragmas = map[string]bool{
	"server-version": true, "send-submanifests": true, "avoid-submanifests": true,
	"ci-lock-fail": true, "uv-push-ok": true, "uv-pull-only": true,
}

// hzPeerStore is a content-horizon clone of g's origin at g.c, open to nobody.
func hzPeerStore(t *testing.T, g *hzSyncGraph) (*repo.Repo, *scSyncPeer, error) {
	t.Helper()
	_, store, _, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		return nil, nil, err
	}
	hzPeerOpen(t, store)
	return store, &scSyncPeer{r: store}, nil
}

// A content-horizon store at C serves a horizon pull at C and refuses one at
// an earlier C, whose held set it does not hold, with one error card and
// nothing else. A go-libfossil client at that earlier C, pulling or
// syncing, takes the refusal as fatal and stores nothing.
func hzPeerRefusePull(t *testing.T) error {
	g := newHZSyncGraph(t)
	store, server, err := hzPeerStore(t, g)
	if err != nil {
		return err
	}
	held, err := server.Exchange(context.Background(), hzPeerPull(t, store, hzUUID(t, g.f.r, g.c3), "descendants"))
	if err != nil {
		return err
	}
	if err := hzPeerServed(held); err != nil {
		return expectf("a pull at the store's own C: %v", err)
	}
	earlier := hzUUID(t, g.f.r, g.c2)
	// A request that asks for pull-side cards of every kind: the version
	// answer, the unversioned catalog, configuration and the clusters.
	refused, err := server.Exchange(context.Background(), hzPeerPull(t, store, earlier, "descendants",
		&xfer.PragmaCard{Name: "client-version", Values: []string{"22800", "20260101", "000000"}},
		&xfer.PragmaCard{Name: "uv-hash", Values: []string{strings.Repeat("0", 40)}},
		&xfer.PragmaCard{Name: "req-clusters"}, &xfer.ReqConfigCard{Name: "/project"}))
	if err != nil {
		return err
	}
	if err := hzPeerRefused(refused); err != nil {
		return expectf("a pull at an earlier C: %v", err)
	}
	if err := scSyncRefused(refused, hzPeerRefusal); err != nil {
		return expectf("a pull at an earlier C: %v", err)
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
		if err == nil || !strings.Contains(err.Error(), hzPeerRefusalPrefix) {
			return expectf("a client at an earlier C (push %v): %v, want the refusal as fatal", opts.Push, err)
		}
	}
	if after := hzBlobCount(t, relay); after != before {
		return expectf("the refused client went from %d blob rows to %d", before, after)
	}
	return nil
}

// A sync refused by §6.4 still takes the peer's push: a client at an
// earlier C with a check-in of its own syncs with the store through the real
// client, whose first round only announces it. The store asks for it, the
// client sends it in a later round and then fails with the refusal, and is
// sent no content: §6.4's "no content" is what the store sends, not what it
// accepts.
func hzPeerRefusePush(t *testing.T) error {
	g := newHZSyncGraph(t)
	_, server, err := hzPeerStore(t, g)
	if err != nil {
		return err
	}
	earlier := hzStoreCone(t, hzUUID(t, g.f.r, g.c2), cone.RuleDescendants, cone.DepthContent)
	_, relay, _, err := hzServeClone(t, g, earlier, sync.HandleOpts{})
	if err != nil {
		return err
	}
	var parent libfossil.FslID
	if err := relay.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", hzUUID(t, g.f.r, g.m)).Scan(&parent); err != nil {
		return err
	}
	rid, _, err := manifest.Checkin(relay, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "relay", Content: []byte("relay\n")}},
		Comment: "relay", User: "u", Parent: parent, Time: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		return err
	}
	pushed := hzUUID(t, relay, rid)
	result, err := sync.Sync(context.Background(), relay, server, sync.SyncOpts{Pull: true, Push: true})
	if err == nil || !strings.Contains(err.Error(), hzPeerRefusalPrefix) {
		return expectf("the refused sync returned %v, want the refusal", err)
	}
	if result.FilesRecvd != 0 || len(hzSent(server.replies)) != 0 {
		return expectf("the refused peer was sent %d files", result.FilesRecvd)
	}
	if err := hzPeerRefused(server.replies[0]); err != nil {
		return expectf("the first reply: %v", err)
	}
	if err := hzHeld(server.r, map[string]bool{pushed: true}, true); err != nil {
		return expectf("the push did not land: %v", err)
	}
	return hzPeerPushError(t, g)
}

// hzPeerPushError: a push the requester may not make keeps its error ahead
// of the refusal, so the peer learns both.
func hzPeerPushError(t *testing.T, g *hzSyncGraph) error {
	store, server, err := hzPeerStore(t, g)
	if err != nil {
		return err
	}
	if _, err := store.DB().Exec("UPDATE user SET cap='go' WHERE login='nobody'"); err != nil {
		return err
	}
	code, err := store.Config("project-code")
	if err != nil {
		return err
	}
	reply, err := server.Exchange(context.Background(), hzPeerPull(t, store, hzUUID(t, g.f.r, g.c2), "descendants",
		&xfer.PushCard{ServerCode: "x", ProjectCode: code}))
	if err != nil {
		return err
	}
	if err := hzPeerRefused(reply); err != nil {
		return err
	}
	for _, card := range reply.Cards {
		if c, ok := card.(*xfer.ErrorCard); ok && c.Message == "not authorized to write" {
			return nil
		}
	}
	return expectf("the push error did not survive the refusal: %v", reply.Cards)
}

// A peer at a later C is served, both one whose C the store holds, whose
// held set the store's walks place inside its own, and one whose C the
// store does not hold yet, whose sync then pushes that check-in in.
func hzPeerLaterC(t *testing.T) error {
	g := newHZSyncGraph(t)
	store, server, err := hzPeerStore(t, g)
	if err != nil {
		return err
	}
	for _, c := range []string{hzUUID(t, g.f.r, g.c4), strings.Repeat("ab", 20)} {
		reply, err := server.Exchange(context.Background(), hzPeerPull(t, store, c, "descendants"))
		if err != nil {
			return err
		}
		if err := hzPeerServed(reply); err != nil {
			return expectf("a pull at later C %s: %v", c, err)
		}
	}
	c5 := hzUUID(t, g.f.r, hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil))
	_, relay, _, err := hzServeClone(t, g, hzStoreCone(t, c5, cone.RuleDescendants, cone.DepthContent),
		sync.HandleOpts{})
	if err != nil {
		return err
	}
	result, err := sync.Sync(context.Background(), relay, server, sync.SyncOpts{Pull: true, Push: true})
	if err != nil || len(result.Errors) != 0 {
		return expectf("a sync at a C the store does not hold: %v, errors %q", err, result.Errors)
	}
	return hzHeld(store, map[string]bool{c5: true}, true)
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

// hzPeerRemedy fails unless diagnostics carry one fallback remedy naming C
// and the dense origin's history.
func hzPeerRemedy(diagnostics []string, c string) error {
	var found []string
	for _, d := range diagnostics {
		if strings.HasPrefix(d, hzPeerFallbackPrefix) {
			found = append(found, d)
		}
	}
	if len(found) != 1 || !strings.Contains(found[0], c) || !strings.Contains(found[0], hzPeerHistory) {
		return expectf("fallback remedies %q, want one naming C %s and the dense origin %s", found, c, hzPeerHistory)
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
	return hzPeerRemedy(result.Messages, x.c3)
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
	if err := hzPeerRemedy(result.Errors, x.c3); err != nil {
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
