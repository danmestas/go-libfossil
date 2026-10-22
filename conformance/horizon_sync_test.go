package conformance

import (
	"context"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gofossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The content horizon's synchronization against a horizon-ignorant peer
// (draft-fossil-horizon-00 §6.3): the peer serves densely and the client
// classifies locally. The peers are go-libfossil's handler, with the
// horizon pragma stripped from what it reads, and fossil(1) through
// `fossil http`.

var (
	horizonSyncClonePull = Case{ID: "horizon.6.3.clone-pull", Req: "HZ-6.3-1"}
	horizonSyncFossil    = Case{ID: "horizon.6.3.fossil-clone-pull", Req: "HZ-6.3-1"}
	horizonSyncTransit   = Case{ID: "horizon.4.2.transit-delta", Req: "HZ-4.2-2"}
	horizonSyncDeepen    = Case{ID: "horizon.4.4.deepen", Req: "HZ-4.4-1"}
	horizonSyncDemand    = Case{ID: "horizon.4.4.demand-fetch", Req: "HZ-4.4-1"}
	horizonSyncNoCluster = Case{ID: "horizon.4.1.no-cluster", Req: "HZ-4.1-1"}
	horizonSyncClusters  = Case{ID: "horizon.4.5.clusters", Req: "HZ-4.5-1"}
)

func horizonSyncCases() []Case {
	return []Case{
		horizonSyncClonePull, horizonSyncFossil, horizonSyncTransit, horizonSyncDeepen,
		horizonSyncDemand, horizonSyncNoCluster, horizonSyncClusters,
	}
}

func TestHorizonSync(t *testing.T) {
	check(t, horizonSyncClonePull, func() error { return hzSyncClonePull(t, false) })
	check(t, horizonSyncFossil, func() error { return hzSyncClonePull(t, true) })
	check(t, horizonSyncTransit, func() error {
		if err := hzSyncTransit(t, false); err != nil {
			return err
		}
		return hzSyncTransit(t, true)
	})
	check(t, horizonSyncDeepen, func() error {
		if err := hzSyncDeepen(t, false); err != nil {
			return err
		}
		return hzSyncDeepen(t, true)
	})
	check(t, horizonSyncDemand, func() error { return hzSyncDemand(t) })
	check(t, horizonSyncNoCluster, func() error {
		if err := hzSyncNoCluster(t, false); err != nil {
			return err
		}
		return hzSyncNoCluster(t, true)
	})
	check(t, horizonSyncClusters, func() error { return hzSyncClusters(t) })
}

// hzPeer is a dense origin that records the decoded requests it is sent.
type hzPeer interface {
	sync.Transport
	sent() []*xfer.Message
	received() []*xfer.Message
}

func (p *scSyncPeer) sent() []*xfer.Message     { return p.requests }
func (p *scSyncPeer) received() []*xfer.Message { return p.replies }

// hzFossilPeer is fossil(1) serving origin, one `fossil http` per exchange.
// The origin's write-ahead log is checkpointed first: fossil(1) refuses a
// repository file that does not yet hold the committed pages.
type hzFossilPeer struct {
	ref      *fossilReference
	origin   *repo.Repo
	requests []*xfer.Message
	replies  []*xfer.Message
}

func (p *hzFossilPeer) sent() []*xfer.Message     { return p.requests }
func (p *hzFossilPeer) received() []*xfer.Message { return p.replies }

func (p *hzFossilPeer) Exchange(_ context.Context, req *xfer.Message) (*xfer.Message, error) {
	req, err := scSyncWire(req)
	if err != nil {
		return nil, err
	}
	p.requests = append(p.requests, req)
	data, err := req.Encode()
	if err != nil {
		return nil, err
	}
	if _, err := p.origin.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return nil, err
	}
	status, ctype, raw, err := p.ref.serveEntity(p.origin.Path(),
		syncRequest{method: "POST", path: "/", ctype: typeCompressed, raw: data})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, expectf("fossil http answered %d: %.200q", status, raw)
	}
	resp, err := xfer.Decode(raw, ctype)
	if err != nil {
		return nil, err
	}
	p.replies = append(p.replies, resp)
	return resp, nil
}

// newHZPeer serves origin densely: through go-libfossil's handler, which
// never sees the request's cone, or through fossil(1), which is nil when the
// binary is absent and not required.
func newHZPeer(t *testing.T, origin *repo.Repo, fossil bool) hzPeer {
	t.Helper()
	if !fossil {
		return &scSyncPeer{r: origin, ignore: true}
	}
	ref := newFossilReference(t)
	if ref == nil {
		return nil
	}
	return &hzFossilPeer{ref: ref, origin: origin}
}

// hzSyncGraph is the horizon graph with C = c3 under the descendants rule:
// its held set, the blobs that set names and the blobs below it.
type hzSyncGraph struct {
	*hzGraph
	c            *cone.Cone
	above, below map[string]bool
}

func newHZSyncGraph(t *testing.T) *hzSyncGraph {
	t.Helper()
	g := &hzSyncGraph{hzGraph: newHZGraph(t)}
	g.c = hzStoreCone(t, hzUUID(t, g.f.r, g.c3), cone.RuleDescendants, cone.DepthContent)
	g.above = hzFiles(t, g.f.r, g.c3, g.c4, g.m, g.side1, g.side2)
	g.below = map[string]bool{}
	for _, uuid := range hzMinus(hzFiles(t, g.f.r, g.checkins()...), g.above) {
		g.below[uuid] = true
	}
	if len(g.above) == 0 || len(g.below) == 0 {
		t.Fatalf("fixture: %d blobs above and %d below the horizon", len(g.above), len(g.below))
	}
	return g
}

// hzHeld reports, per name, whether r can read it.
func hzHeld(r *repo.Repo, uuids map[string]bool, want bool) error {
	for _, uuid := range scSorted(uuids) {
		_, got, err := content.CheckAvailableByUUID(r.DB(), uuid)
		if err != nil {
			return err
		}
		if got != want {
			return expectf("holding %s = %v, want %v", uuid, got, want)
		}
	}
	return nil
}

// hzElided checks that r classifies exactly want as elided, holds no
// transit phantom, and verifies clean while reporting the elision.
func hzElided(r *repo.Repo, want map[string]bool) error {
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		return err
	}
	classes := map[string]content.Absence{}
	rows, err := r.DB().Query("SELECT p.rid, b.uuid FROM phantom p JOIN blob b ON b.rid=p.rid")
	if err != nil {
		return err
	}
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			rows.Close()
			return err
		}
		classes[uuid] = a.Class(rid)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := scSameList("elided", scClassed(classes, content.Elided), scSorted(want)); err != nil {
		return err
	}
	if got := scClassed(classes, content.Transit); len(got) != 0 {
		return expectf("transit phantoms %v, want none", got)
	}
	report, err := verify.Verify(r)
	if err != nil {
		return err
	}
	if !report.OK() || report.Elided != len(want) || report.Phantoms != 0 {
		return expectf("verify ok=%v elided=%d phantoms=%d issues=%v, want ok, %d and 0",
			report.OK(), report.Elided, report.Phantoms, report.Issues, len(want))
	}
	return nil
}

// hzNoGimme checks that no request asked for any of uuids.
func hzNoGimme(requests []*xfer.Message, uuids map[string]bool) error {
	for uuid := range uuids {
		if err := scSyncNoGimme(requests, uuid); err != nil {
			return err
		}
	}
	return nil
}

func hzSyncCloneHorizon(t *testing.T, g *hzSyncGraph, peer hzPeer) (*repo.Repo, *sync.CloneResult, error) {
	t.Helper()
	r, result, err := sync.Clone(context.Background(),
		filepath.Join(t.TempDir(), "horizon.fossil"), peer, sync.CloneOpts{StoreCone: g.c, PullPhantoms: true})
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(func() { r.Close() })
	return r, result, nil
}

// A dense peer's clone sends every blob: the clone keeps those its held set
// names, records the rest elided, holds every manifest and reports the
// count. A later pull keeps a new check-in above the horizon and elides
// the blob of one below it; once classified, no elided blob is requested.
func hzSyncClonePull(t *testing.T, fossil bool) error {
	g := newHZSyncGraph(t)
	peer := newHZPeer(t, g.f.r, fossil)
	if peer == nil {
		return nil
	}
	r, cloned, err := hzSyncCloneHorizon(t, g, peer)
	if err != nil {
		return err
	}
	if cloned.Elided != len(g.below) {
		return expectf("clone elided=%d, want %d", cloned.Elided, len(g.below))
	}
	if err := hzHeld(r, g.above, true); err != nil {
		return err
	}
	if err := hzHeld(r, g.below, false); err != nil {
		return err
	}
	manifests := map[string]bool{}
	for _, rid := range g.checkins() {
		manifests[hzUUID(t, g.f.r, rid)] = true
	}
	if err := hzHeld(r, manifests, true); err != nil {
		return err
	}
	if err := hzElided(r, g.below); err != nil {
		return err
	}
	cloneRequests := len(peer.sent())
	above := hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	below := hzCommit(t, g.f, "feat2", []libfossil.FslID{g.feat}, nil)
	newAbove := hzMinus(hzFiles(t, g.f.r, above), g.above)
	newBelow := hzMinus(hzFiles(t, g.f.r, below), hzFiles(t, g.f.r, g.checkins()...))
	if len(newAbove) != 1 || len(newBelow) != 1 {
		return expectf("fixture: new blobs above %v below %v", newAbove, newBelow)
	}
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if pulled.Elided != len(g.below)+1 || len(pulled.Errors) != 0 {
		return expectf("pull elided=%d errors=%q, want %d and none", pulled.Elided, pulled.Errors, len(g.below)+1)
	}
	if err := hzHeld(r, map[string]bool{newAbove[0]: true,
		hzUUID(t, g.f.r, above): true, hzUUID(t, g.f.r, below): true}, true); err != nil {
		return err
	}
	if err := hzHeld(r, map[string]bool{newBelow[0]: true}, false); err != nil {
		return err
	}
	if err := hzNoGimme(peer.sent()[cloneRequests:], g.below); err != nil {
		return err
	}
	// The pull met feat2's blob in the peer's inventory before the manifest
	// that names it; once classified, it is not asked for again.
	stable := len(peer.sent())
	elided := map[string]bool{newBelow[0]: true}
	for uuid := range g.below {
		elided[uuid] = true
	}
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	if err := hzNoGimme(peer.sent()[stable:], elided); err != nil {
		return err
	}
	return hzElided(r, elided)
}

// hzDeltaPeer is a dense peer that sends target as a delta against base,
// which neither peer under test does in a clone, and base in full; while
// withhold is set it leaves base's card out of its replies.
type hzDeltaPeer struct {
	*scSyncPeer
	target, base string
	delta, full  []byte
	withhold     bool
}

func (p *hzDeltaPeer) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp, err := p.scSyncPeer.Exchange(ctx, req)
	if err != nil {
		return nil, err
	}
	cards := resp.Cards[:0]
	for _, card := range resp.Cards {
		var uuid string
		switch c := card.(type) {
		case *xfer.FileCard:
			uuid = c.UUID
		case *xfer.CFileCard:
			uuid = c.UUID
		}
		switch {
		case uuid == p.target:
			card = &xfer.FileCard{UUID: p.target, DeltaSrc: p.base, Content: p.delta}
		case uuid == p.base && p.withhold:
			continue
		case uuid == p.base:
			card = &xfer.FileCard{UUID: p.base, Content: p.full}
		}
		cards = append(cards, card)
	}
	resp.Cards = cards
	return resp, nil
}

// A held blob that arrives as a delta against a blob below the horizon
// makes that base transit whatever its class: the clone requests it and
// keeps it. A base held when the delta arrives resolves it whole, and is
// then elided like any blob below the horizon.
func hzSyncTransit(t *testing.T, withhold bool) error {
	base := strings.Repeat("base content\n", 1000)
	child := strings.Repeat("base content\n", 999) + "changed\n"
	origin := newSCSyncRepo(t, map[string]string{"a": base})
	c := origin.commit(t, map[string]string{"a": child}, origin.c1)
	baseUUID, childUUID := scUUIDOf(t, origin.r, origin.c1, "a"), scUUIDOf(t, origin.r, c, "a")
	d, err := delta.Create([]byte(base), []byte(child))
	if err != nil {
		return err
	}
	peer := &hzDeltaPeer{scSyncPeer: &scSyncPeer{r: origin.r, ignore: true},
		target: childUUID, base: baseUUID, delta: d, full: []byte(base), withhold: withhold}
	sc := hzStoreCone(t, hzUUID(t, origin.r, c), cone.RuleDescendants, cone.DepthContent)
	r, result, err := sync.Clone(context.Background(),
		filepath.Join(t.TempDir(), "horizon.fossil"), peer, sync.CloneOpts{StoreCone: sc})
	if err != nil {
		return err
	}
	defer r.Close()
	if !withhold {
		// The delta resolved against a base already held, so the child is
		// stored whole and the base, below the horizon, is not needed.
		if result.Elided != 1 {
			return expectf("clone elided=%d, want 1", result.Elided)
		}
		if err := hzHeld(r, map[string]bool{childUUID: true}, true); err != nil {
			return err
		}
		return hzElided(r, map[string]bool{baseUUID: true})
	}
	if result.Elided != 0 {
		return expectf("clone elided=%d, want 0: the base is a held delta's", result.Elided)
	}
	if err := hzHeld(r, map[string]bool{baseUUID: true}, false); err != nil {
		return err
	}
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		return err
	}
	baseRID, _ := blob.Exists(r.DB(), baseUUID)
	if got := a.Class(baseRID); got != content.Transit {
		return expectf("a held delta's withheld base is %v, want transit", got)
	}
	peer.withhold = false
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if pulled.Elided != 0 || len(pulled.Errors) != 0 {
		return expectf("pull elided=%d errors=%q, want 0 and none", pulled.Elided, pulled.Errors)
	}
	if err := hzHeld(r, map[string]bool{childUUID: true, baseUUID: true}, true); err != nil {
		return err
	}
	return hzElided(r, map[string]bool{})
}

// Moving C to an ancestor reclassifies the blobs the new held set names as
// transit, and the next ordinary pull fetches exactly those.
func hzSyncDeepen(t *testing.T, fossil bool) error {
	g := newHZSyncGraph(t)
	peer := newHZPeer(t, g.f.r, fossil)
	if peer == nil {
		return nil
	}
	r, _, err := hzSyncCloneHorizon(t, g, peer)
	if err != nil {
		return err
	}
	deeper := hzStoreCone(t, hzUUID(t, g.f.r, g.c2), cone.RuleDescendants, cone.DepthContent)
	if err := content.SetStoreCone(r.DB(), deeper); err != nil {
		return err
	}
	kept := map[string]bool{}
	for _, uuid := range hzMinus(hzFiles(t, g.f.r, g.c2), g.above) {
		kept[uuid] = true
	}
	if len(kept) == 0 {
		return expectf("fixture: deepening to c2 keeps no new blob")
	}
	before := len(peer.sent())
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if len(pulled.Errors) != 0 {
		return expectf("deepening pull errors=%q", pulled.Errors)
	}
	if err := hzHeld(r, kept, true); err != nil {
		return err
	}
	still := map[string]bool{}
	for uuid := range g.below {
		if !kept[uuid] {
			still[uuid] = true
		}
	}
	if err := hzNoGimme(peer.sent()[before:], still); err != nil {
		return err
	}
	if pulled.Elided != len(still) {
		return expectf("deepening pull elided=%d, want %d", pulled.Elided, len(still))
	}
	return hzElided(r, still)
}

// Updating a checkout to a check-in below C fetches exactly the blobs that
// check-in needs from the explicit peer; without one it is refused.
func hzSyncDemand(t *testing.T) error {
	g := newHZSyncGraph(t)
	server := httptest.NewServer(sync.XferHandler(g.f.r, sync.HandleSync))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "horizon.fossil")
	r, _, err := sync.Clone(context.Background(), path, &sync.HTTPTransport{URL: server.URL},
		sync.CloneOpts{StoreCone: g.c, PullPhantoms: true})
	if err != nil {
		return err
	}
	if err := r.Close(); err != nil {
		return err
	}
	pub, err := gofossil.Open(path)
	if err != nil {
		return err
	}
	defer pub.Close()
	co, err := pub.CreateCheckout(filepath.Join(t.TempDir(), "co"), gofossil.CheckoutCreateOpts{})
	if err != nil {
		return err
	}
	defer co.Close()
	// The clone numbers its rows in the order they arrived, not the origin's.
	c1, ok := blob.Exists(pub.Inner().DB(), hzUUID(t, g.f.r, g.c1))
	if !ok {
		return expectf("the clone does not name c1")
	}
	target := gofossil.UpdateOpts{TargetRID: int64(c1)}
	if _, err := co.UpdateContext(context.Background(), target, gofossil.RequiredContentOpts{}); err == nil {
		return expectf("an update below the horizon with no peer succeeded")
	}
	if _, err := co.UpdateContext(context.Background(), target,
		gofossil.RequiredContentOpts{Peer: gofossil.NewHTTPTransport(server.URL)}); err != nil {
		return expectf("an update below the horizon with a peer: %v", err)
	}
	needed := hzFiles(t, g.f.r, g.c1)
	if err := hzHeld(pub.Inner(), needed, true); err != nil {
		return err
	}
	got, err := os.ReadFile(filepath.Join(co.Dir(), "src", "a"))
	if err != nil || string(got) != "inside\n" {
		return expectf("src/a after the update = %q, %v", got, err)
	}
	unfetched := map[string]bool{}
	for uuid := range g.below {
		if !needed[uuid] {
			unfetched[uuid] = true
		}
	}
	return hzHeld(pub.Inner(), unfetched, false)
}

// hzClustered stores n blobs no artifact references, then clusters every
// unclustered artifact of origin, and returns the blobs and the clusters.
func hzClustered(t *testing.T, origin *repo.Repo, tag string, n int) (map[string]bool, map[string]bool) {
	t.Helper()
	members := map[string]bool{}
	clusters := map[string]bool{}
	err := origin.WithTx(func(tx *db.Tx) error {
		for i := range n {
			_, uuid, err := blob.Store(tx, fmt.Appendf(nil, "%s unreferenced %d\n", tag, i))
			if err != nil {
				return err
			}
			members[uuid] = true
		}
		made, err := content.GenerateClusters(tx)
		if err == nil && made == 0 {
			err = fmt.Errorf("no cluster minted")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := origin.DB().Query("SELECT b.uuid FROM tagxref x JOIN blob b ON b.rid=x.rid WHERE x.tagid=7")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			t.Fatal(err)
		}
		clusters[uuid] = true
	}
	return members, clusters
}

// hzReqClusters reports whether a request asked for the cluster catalog.
func hzReqClusters(requests []*xfer.Message) bool {
	for _, m := range requests {
		for _, card := range m.Cards {
			if c, ok := card.(*xfer.PragmaCard); ok && c.Name == "req-clusters" {
				return true
			}
		}
	}
	return false
}

// A horizon clone holds no cluster it is sent, so a later sync neither
// requests one back nor asks for the catalog, and it mints none.
func hzSyncNoCluster(t *testing.T, fossil bool) error {
	g := newHZSyncGraph(t)
	_, clusters := hzClustered(t, g.f.r, "old", content.ClusterThreshold)
	peer := newHZPeer(t, g.f.r, fossil)
	if peer == nil {
		return nil
	}
	r, _, err := hzSyncCloneHorizon(t, g, peer)
	if err != nil {
		return err
	}
	if err := hzHeld(r, clusters, false); err != nil {
		return err
	}
	before := len(peer.sent())
	for range 2 {
		if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true, Push: true}); err != nil {
			return err
		}
	}
	if hzReqClusters(peer.sent()) {
		return expectf("a horizon clone asked for the cluster catalog")
	}
	if err := hzNoGimme(peer.sent()[before:], clusters); err != nil {
		return err
	}
	var minted int
	if err := r.DB().QueryRow(`SELECT count(*) FROM tagxref x JOIN blob b ON b.rid=x.rid
		WHERE x.tagid=7 AND b.size>=0`).Scan(&minted); err != nil {
		return err
	}
	if minted != 0 {
		return expectf("the horizon clone holds %d cluster(s)", minted)
	}
	return hzElided(r, hzUnion(g.below, clusters))
}

func hzUnion(a, b map[string]bool) map[string]bool {
	out := maps.Clone(a)
	maps.Copy(out, b)
	return out
}

// A pull that meets a new cluster requests it (an igot names it) but not
// the artifacts only it names, while a dense clone of the same origin
// does. A clustered check-in a new one names by its P card is requested,
// and so is the blob its F card names. The peer is fossil(1), which
// announces unclustered artifacts alone: go-libfossil's handler, serving
// densely, announces every artifact by igot, which a horizon clone
// requests (§4.5).
func hzSyncClusters(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer := newHZPeer(t, g.f.r, true)
	if peer == nil {
		return nil
	}
	r, _, err := hzSyncCloneHorizon(t, g, peer)
	if err != nil {
		return err
	}
	dense, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "dense.fossil"),
		peer, sync.CloneOpts{})
	if err != nil {
		return err
	}
	defer dense.Close()
	parent := hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	members, clusters := hzClustered(t, g.f.r, "new", content.ClusterThreshold)
	tip := hzCommit(t, g.f, "c6", []libfossil.FslID{parent}, nil)
	named := map[string]bool{hzUUID(t, g.f.r, parent): true,
		scUUIDOf(t, g.f.r, parent, "c5"): true, scUUIDOf(t, g.f.r, tip, "c6"): true}
	before := len(peer.sent())
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	if err := hzNoGimme(peer.sent()[before:], members); err != nil {
		return err
	}
	for uuid := range members {
		if _, known := blob.Exists(r.DB(), uuid); known {
			return expectf("a cluster-only member %s became a phantom", uuid)
		}
	}
	if err := hzHeld(r, clusters, false); err != nil {
		return err
	}
	stable := len(peer.sent())
	if _, err := sync.Sync(context.Background(), dense, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	if err := hzNoGimme(peer.sent()[stable:], members); err == nil {
		return expectf("fixture: a dense clone did not request the cluster's members")
	}
	stable = len(peer.sent())
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	if err := hzNoGimme(peer.sent()[stable:], hzUnion(members, clusters)); err != nil {
		return err
	}
	// The blob c5 names is learned when c5 is crosslinked, after the
	// round that brought it: the second pull asks for it.
	if err := hzHeld(r, named, true); err != nil {
		return err
	}
	return hzElided(r, hzUnion(g.below, clusters))
}
