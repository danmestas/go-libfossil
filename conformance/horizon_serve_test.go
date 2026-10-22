package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The horizon pragmas and the origin that serves them
// (draft-fossil-horizon-00 §6.1, §6.2, §6.2.1, §6.2.5, §6.4): go-libfossil's
// handler is the origin, its clone and pull the client. What crosses the
// wire is read from the replies the peer recorded.

var (
	horizonServeRequest  = Case{ID: "horizon.6.1.request-pragma", Req: "HZ-6.1-1"}
	horizonServeAbove    = Case{ID: "horizon.6.2.above-only", Req: "HZ-6.2-1"}
	horizonServePaths    = Case{ID: "horizon.6.2.path-intersection", Req: "HZ-6.2-1"}
	horizonServeTree     = Case{ID: "horizon.6.2.tree", Req: "HZ-6.2-1"}
	horizonServeRounds   = Case{ID: "horizon.8.round-bound", Req: "HZ-8-1"}
	horizonServeClusters = Case{ID: "horizon.6.2.no-cluster", Req: "HZ-6.2-2"}
	horizonServeInflate  = Case{ID: "horizon.6.2.1.inflate", Req: "HZ-6.2-3"}
	horizonServeServed   = Case{ID: "horizon.6.2.5.served", Req: "HZ-6.2-8"}
	horizonServeIgnored  = Case{ID: "horizon.6.3.served-absent", Req: "HZ-6.3-1"}
	horizonServeRefuse   = Case{ID: "horizon.6.4.refuse", Req: "HZ-6.4-1"}
	horizonServeNoChase  = Case{ID: "horizon.4.5.served-no-cluster-chase", Req: "HZ-4.5-1"}
)

func horizonServeCases() []Case {
	return []Case{
		horizonServeRequest, horizonServeAbove, horizonServePaths, horizonServeTree, horizonServeRounds,
		horizonServeClusters, horizonServeInflate, horizonServeServed, horizonServeIgnored,
		horizonServeRefuse, horizonServeNoChase,
	}
}

func TestHorizonServe(t *testing.T) {
	check(t, horizonServeRequest, func() error { return hzServeRequest(t) })
	check(t, horizonServeAbove, func() error { return hzServeAbove(t) })
	check(t, horizonServePaths, func() error { return hzServePaths(t) })
	check(t, horizonServeTree, func() error { return hzServeTree(t) })
	check(t, horizonServeRounds, func() error { return hzServeRounds(t) })
	check(t, horizonServeClusters, func() error { return hzServeClusters(t) })
	check(t, horizonServeInflate, func() error { return hzServeInflate(t) })
	check(t, horizonServeServed, func() error { return hzServeServed(t) })
	check(t, horizonServeIgnored, func() error { return hzServeIgnored(t) })
	check(t, horizonServeRefuse, func() error { return hzServeRefuse(t) })
	check(t, horizonServeNoChase, func() error { return hzServeNoChase(t) })
}

// hzPragmas returns the values of every pragma named name in m.
func hzPragmas(m *xfer.Message, name string) [][]string {
	var out [][]string
	for _, card := range m.Cards {
		if p, ok := card.(*xfer.PragmaCard); ok && p.Name == name {
			out = append(out, p.Values)
		}
	}
	return out
}

// hzSent is every artifact a reply carried as a file or cfile card, by
// name, with the delta source it named ("" for full form).
func hzSent(replies []*xfer.Message) map[string]string {
	out := map[string]string{}
	for _, m := range replies {
		for uuid, src := range scSyncPayloads(m) {
			out[uuid] = src
		}
	}
	return out
}

// hzAnnounced is every name a reply announced with igot.
func hzAnnounced(replies []*xfer.Message) map[string]bool {
	out := map[string]bool{}
	for _, m := range replies {
		for _, card := range m.Cards {
			if c, ok := card.(*xfer.IGotCard); ok {
				out[c.UUID] = true
			}
		}
	}
	return out
}

// hzCrossed fails when any of uuids crossed the wire, sent or announced.
func hzCrossed(replies []*xfer.Message, uuids map[string]bool) error {
	sent, announced := hzSent(replies), hzAnnounced(replies)
	for _, uuid := range scSorted(uuids) {
		if _, ok := sent[uuid]; ok {
			return expectf("the origin sent %s", uuid)
		}
		if announced[uuid] {
			return expectf("the origin announced %s", uuid)
		}
	}
	return nil
}

// hzServeClone clones g's origin under sc through a horizon-aware peer.
func hzServeClone(t *testing.T, g *hzSyncGraph, sc *cone.Cone, opts sync.HandleOpts) (*scSyncPeer, *repo.Repo, *sync.CloneResult, error) {
	t.Helper()
	peer := &scSyncPeer{r: g.f.r, opts: opts}
	r, result, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "horizon.fossil"),
		peer, sync.CloneOpts{StoreCone: sc})
	if err != nil {
		return nil, nil, nil, err
	}
	t.Cleanup(func() { r.Close() })
	return peer, r, result, nil
}

// Every clone and pull request of a horizon store carries one horizon
// pragma naming C and the rule, after its path-cone pragmas, and never
// horizon-structural; a dense store's requests carry none.
func hzServeRequest(t *testing.T) error {
	g := newHZSyncGraph(t)
	c := hzUUID(t, g.f.r, g.c3)
	pathed := hzStoreCone(t, c, cone.RuleLeaves, cone.DepthContent, "src")
	for _, sc := range []*cone.Cone{g.c, pathed, nil} {
		peer, r, _, err := hzServeClone(t, g, sc, sync.HandleOpts{})
		if err != nil {
			return err
		}
		if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
			return err
		}
		for i, req := range peer.requests {
			got := hzPragmas(req, "horizon")
			if len(hzPragmas(req, "horizon-structural")) != 0 {
				return expectf("request %d asks for the structural horizon", i)
			}
			if sc == nil {
				if len(got) != 0 {
					return expectf("dense request %d carries %q", i, got)
				}
				continue
			}
			h, _ := sc.Horizon()
			if len(got) != 1 || strings.Join(got[0], " ") != c+" "+h.Rule().String() {
				return expectf("request %d carries horizon pragmas %q, want one %q", i, got, c+" "+h.Rule().String())
			}
			if err := hzAfterCone(req); err != nil {
				return expectf("request %d: %v", i, err)
			}
		}
	}
	return nil
}

// hzAfterCone fails when a path-cone pragma follows the horizon pragma.
func hzAfterCone(m *xfer.Message) error {
	seen := false
	for _, card := range m.Cards {
		p, ok := card.(*xfer.PragmaCard)
		switch {
		case !ok:
		case p.Name == "horizon":
			seen = true
		case seen && (p.Name == "cone" || p.Name == "cone-not"):
			return expectf("pragma %s follows the horizon pragma", p.Name)
		}
	}
	return nil
}

// The origin sends every manifest and every blob above the horizon, and
// no blob below it: not in the clone, nor in a later pull, whose new
// check-in below the horizon is sent without its blob.
func hzServeAbove(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer, r, cloned, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		return err
	}
	sent := hzSent(peer.replies)
	for _, rid := range g.checkins() {
		if _, ok := sent[hzUUID(t, g.f.r, rid)]; !ok {
			return expectf("the clone was not sent manifest rid=%d", rid)
		}
	}
	for uuid := range g.above {
		if _, ok := sent[uuid]; !ok {
			return expectf("the clone was not sent %s, above the horizon", uuid)
		}
	}
	if err := hzCrossed(peer.replies, g.below); err != nil {
		return err
	}
	if cloned.Elided != len(g.below) {
		return expectf("clone elided=%d, want %d", cloned.Elided, len(g.below))
	}
	if err := hzElided(r, g.below); err != nil {
		return err
	}
	above := hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	below := hzCommit(t, g.f, "feat2", []libfossil.FslID{g.feat}, nil)
	newAbove := hzMinus(hzFiles(t, g.f.r, above), g.above)
	newBelow := hzMinus(hzFiles(t, g.f.r, below), hzFiles(t, g.f.r, g.checkins()...))
	if len(newAbove) != 1 || len(newBelow) != 1 {
		return expectf("fixture: new blobs above %v below %v", newAbove, newBelow)
	}
	before := len(peer.replies)
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if len(pulled.Errors) != 0 {
		return expectf("pull errors %q", pulled.Errors)
	}
	if err := hzCrossed(peer.replies[before:], hzUnion(g.below, map[string]bool{newBelow[0]: true})); err != nil {
		return err
	}
	if err := hzHeld(r, map[string]bool{newAbove[0]: true,
		hzUUID(t, g.f.r, above): true, hzUUID(t, g.f.r, below): true}, true); err != nil {
		return err
	}
	return hzElided(r, hzUnion(g.below, map[string]bool{newBelow[0]: true}))
}

// hzCovered is the blobs the check-ins rids name under a path c covers.
func hzCovered(t *testing.T, r *repo.Repo, c *cone.Cone, rids ...libfossil.FslID) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, rid := range rids {
		files, err := manifest.ListFiles(r, rid)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if c.Covers(f.Name) {
				out[f.UUID] = true
			}
		}
	}
	return out
}

// With path members as well, the blobs sent are those above the horizon
// that an in-cone F card names: the intersection of the two keep sets.
func hzServePaths(t *testing.T) error {
	g := newHZSyncGraph(t)
	c := hzUUID(t, g.f.r, g.c3)
	sc := hzStoreCone(t, c, cone.RuleDescendants, cone.DepthContent, "src", "c4")
	k := []libfossil.FslID{g.c3, g.c4, g.m, g.side1, g.side2}
	keep := hzCovered(t, g.f.r, sc, k...)
	all := hzFiles(t, g.f.r, g.checkins()...)
	if len(keep) == 0 || len(keep) == len(g.above) {
		return expectf("fixture: %d of %d blobs above the horizon are in the paths", len(keep), len(g.above))
	}
	peer, _, _, err := hzServeClone(t, g, sc, sync.HandleOpts{})
	if err != nil {
		return err
	}
	sent := hzSent(peer.replies)
	for uuid := range all {
		if _, got := sent[uuid]; got != keep[uuid] {
			return expectf("blob %s sent=%v, want %v", uuid, got, keep[uuid])
		}
	}
	return nil
}

// Over sub-manifest trees the origin sends every node, of check-ins below
// the horizon too, and the blobs above it only, so the clone holds whole
// trees at every age and no transit phantom.
func hzServeTree(t *testing.T) error {
	origin := newSCSyncTreeRepo(t)
	files := scSyncTreeFiles()
	files["src/a"], files["zzz/deep/c"] = "inside 2\n", "deep 2\n"
	c2 := origin.commit(t, files, origin.c1)
	files["yyy/d"] = "y 3\n"
	origin.commit(t, files, c2)
	below := map[string]bool{scUUIDOf(t, origin.r, origin.c1, "src/a"): true,
		scUUIDOf(t, origin.r, origin.c1, "zzz/deep/c"): true}
	nodes := map[string]bool{}
	for uuid, d := range scSyncTreeOrigin(t, origin.r) {
		if d != nil && d.Type == deck.Submanifest {
			nodes[uuid] = true
		}
	}
	if len(nodes) == 0 {
		return expectf("fixture: the origin holds no sub-manifest node")
	}
	g := &hzSyncGraph{hzGraph: &hzGraph{f: origin}}
	sc := hzStoreCone(t, hzUUID(t, origin.r, c2), cone.RuleDescendants, cone.DepthContent)
	peer, r, _, err := hzServeClone(t, g, sc, sync.HandleOpts{})
	if err != nil {
		return err
	}
	sent := hzSent(peer.replies)
	for uuid := range nodes {
		if _, ok := sent[uuid]; !ok {
			return expectf("the clone was not sent node %s", uuid)
		}
	}
	if err := hzCrossed(peer.replies, below); err != nil {
		return err
	}
	return hzElided(r, below)
}

// A horizon clone answered one artifact per round still converges, each
// reply carrying one artifact and the delta sources it names: the walk is
// bounded as a dense clone's batch is.
func hzServeRounds(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer, r, _, err := hzServeClone(t, g, g.c, sync.HandleOpts{Buggify: scOneArtifactRounds{}})
	if err != nil {
		return err
	}
	for i, reply := range peer.replies {
		full := 0
		for _, src := range scSyncPayloads(reply) {
			if src == "" {
				full++
			}
		}
		if full > 1 {
			return expectf("reply %d carries %d full artifacts, want at most one", i, full)
		}
	}
	if err := hzCrossed(peer.replies, g.below); err != nil {
		return err
	}
	if err := hzHeld(r, g.above, true); err != nil {
		return err
	}
	return hzElided(r, g.below)
}

// No reply to a horizon request carries or announces a cluster: not the
// clone, not a pull, and req-clusters under a horizon pragma is answered
// with nothing, where without one it lists the clusters.
func hzServeClusters(t *testing.T) error {
	g := newHZSyncGraph(t)
	_, clusters := hzClustered(t, g.f.r, "old", content.ClusterThreshold)
	peer, r, _, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		return err
	}
	parent := hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	_, fresh := hzClustered(t, g.f.r, "new", content.ClusterThreshold)
	hzCommit(t, g.f, "c6", []libfossil.FslID{parent}, nil)
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	if err := hzCrossed(peer.replies, hzUnion(clusters, fresh)); err != nil {
		return err
	}
	c := hzUUID(t, g.f.r, g.c3)
	ask := func(cards ...xfer.Card) (int, error) {
		resp, err := sync.HandleSync(context.Background(), g.f.r,
			&xfer.Message{Cards: append(cards, &xfer.PragmaCard{Name: "req-clusters"})})
		if err != nil {
			return 0, err
		}
		return len(hzAnnounced([]*xfer.Message{resp})), nil
	}
	dense, err := ask()
	if err != nil {
		return err
	}
	scoped, err := ask(&xfer.PragmaCard{Name: "horizon", Values: []string{c, "descendants"}})
	if err != nil {
		return err
	}
	if dense == 0 || scoped != 0 {
		return expectf("req-clusters announced %d clusters densely and %d under a horizon, want some and none", dense, scoped)
	}
	return nil
}

// An artifact above the horizon stored as a delta against a blob below it
// is sent in full; a delta whose base the request keeps, a manifest
// against its successor, goes verbatim. A pull answers in full form.
func hzServeInflate(t *testing.T) error {
	base := strings.Repeat("base content\n", 1000)
	child := strings.Repeat("base content\n", 999) + "changed\n"
	files := map[string]string{"a": base}
	for i := range 20 {
		files[fmt.Sprintf("f%02d", i)] = fmt.Sprintf("filler %d\n", i)
	}
	origin := newSCSyncRepo(t, files)
	files["a"] = child
	c := origin.commit(t, files, origin.c1)
	if src, err := content.DeltaSource(origin.r.DB(), origin.c1); err != nil || src != c {
		return expectf("fixture: the first manifest's delta source is %d, %v; want %d", src, err, c)
	}
	baseUUID, childUUID := scUUIDOf(t, origin.r, origin.c1, "a"), scUUIDOf(t, origin.r, c, "a")
	baseRID, _ := blob.Exists(origin.r.DB(), baseUUID)
	childRID, _ := blob.Exists(origin.r.DB(), childUUID)
	if err := origin.r.WithTx(func(tx *db.Tx) error {
		if err := content.Undelta(tx, baseRID); err != nil {
			return err
		}
		_, err := content.Deltify(tx, childRID, baseRID)
		return err
	}); err != nil {
		return err
	}
	if src, err := content.DeltaSource(origin.r.DB(), childRID); err != nil || src != baseRID {
		return expectf("fixture: the child's delta source is %d, %v; want %d", src, err, baseRID)
	}
	g := &hzSyncGraph{hzGraph: &hzGraph{f: origin}}
	sc := hzStoreCone(t, hzUUID(t, origin.r, c), cone.RuleDescendants, cone.DepthContent)
	peer, r, _, err := hzServeClone(t, g, sc, sync.HandleOpts{})
	if err != nil {
		return err
	}
	sent := hzSent(peer.replies)
	if src, ok := sent[childUUID]; !ok || src != "" {
		return expectf("the child went out with delta source %q (sent %v), want full form", src, ok)
	}
	if err := hzCrossed(peer.replies, map[string]bool{baseUUID: true}); err != nil {
		return err
	}
	if src := sent[hzUUID(t, origin.r, origin.c1)]; src != hzUUID(t, origin.r, c) {
		return expectf("the first manifest went out against %q, want its kept successor", src)
	}
	if err := hzHeld(r, map[string]bool{childUUID: true}, true); err != nil {
		return err
	}
	if err := hzElided(r, map[string]bool{baseUUID: true}); err != nil {
		return err
	}
	grand := strings.Repeat("base content\n", 998) + "changed again\n"
	files["a"] = grand
	tip := origin.commit(t, files, c)
	grandUUID := scUUIDOf(t, origin.r, tip, "a")
	grandRID, _ := blob.Exists(origin.r.DB(), grandUUID)
	if err := origin.r.WithTx(func(tx *db.Tx) error {
		if err := content.Undelta(tx, grandRID); err != nil {
			return err
		}
		_, err := content.Deltify(tx, grandRID, baseRID)
		return err
	}); err != nil {
		return err
	}
	before := len(peer.replies)
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	if src, ok := hzSent(peer.replies[before:])[grandUUID]; !ok || src != "" {
		return expectf("the pull sent the new blob with delta source %q (sent %v), want full form", src, ok)
	}
	return hzHeld(r, map[string]bool{grandUUID: true}, true)
}

// hzServedReplies fails unless every reply carries want horizon-served
// content pragmas.
func hzServedReplies(replies []*xfer.Message, want int) error {
	for i, reply := range replies {
		got := hzPragmas(reply, "horizon-served")
		if len(got) != want {
			return expectf("reply %d carries %d horizon-served, want %d", i, len(got), want)
		}
		for _, values := range got {
			if strings.Join(values, " ") != "content" {
				return expectf("reply %d advertises %q, want content", i, values)
			}
		}
	}
	return nil
}

// Every reply to a horizon request says horizon-served content, and the
// client counts the rounds served so; a dense request's replies do not.
func hzServeServed(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer, r, cloned, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		return err
	}
	if cloned.HorizonServed == 0 || cloned.HorizonServed != cloned.Rounds {
		return expectf("clone served %d of %d rounds at the horizon", cloned.HorizonServed, cloned.Rounds)
	}
	hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if pulled.HorizonServed == 0 || pulled.HorizonServed != pulled.Rounds {
		return expectf("pull served %d of %d rounds at the horizon", pulled.HorizonServed, pulled.Rounds)
	}
	if err := hzServedReplies(peer.replies, 1); err != nil {
		return err
	}
	dense, _, densed, err := hzServeClone(t, g, nil, sync.HandleOpts{})
	if err != nil {
		return err
	}
	if densed.HorizonServed != 0 {
		return expectf("a dense clone counted %d horizon rounds", densed.HorizonServed)
	}
	return hzServedReplies(dense.replies, 0)
}

// hzStructuralPeer advertises the structural level in every reply.
type hzStructuralPeer struct{ *scSyncPeer }

func (p hzStructuralPeer) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp, err := p.scSyncPeer.Exchange(ctx, req)
	if err != nil {
		return nil, err
	}
	for _, card := range resp.Cards {
		if c, ok := card.(*xfer.PragmaCard); ok && c.Name == "horizon-served" {
			c.Values = []string{"structural"}
		}
	}
	return resp, nil
}

// A peer that ignores the horizon pragma says nothing, and the client
// classifies locally and counts no served round. A peer claiming the
// structural level does not move the client off the content horizon.
func hzServeIgnored(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer := &scSyncPeer{r: g.f.r, ignore: true}
	r, cloned, err := hzSyncCloneHorizon(t, g, peer)
	if err != nil {
		return err
	}
	if cloned.HorizonServed != 0 {
		return expectf("an ignoring peer's clone counted %d horizon rounds", cloned.HorizonServed)
	}
	if err := hzServedReplies(peer.replies, 0); err != nil {
		return err
	}
	if err := hzElided(r, g.below); err != nil {
		return err
	}
	structural := hzStructuralPeer{&scSyncPeer{r: g.f.r}}
	pulled, err := sync.Sync(context.Background(), r, structural, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if pulled.HorizonServed != 0 {
		return expectf("a structural advertisement counted %d content rounds", pulled.HorizonServed)
	}
	for i, req := range structural.requests {
		if len(hzPragmas(req, "horizon-structural")) != 0 {
			return expectf("request %d entered the structural horizon", i)
		}
	}
	return hzElided(r, g.below)
}

// A server over a horizon store refuses a dense clone and a horizon whose
// held set is not inside its own, with one error card and no content, and
// a requesting client fails; fossil(1) aborts its clone. A horizon whose
// held set is inside the store's is served.
func hzServeRefuse(t *testing.T) error {
	g := newHZSyncGraph(t)
	_, store, _, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		return err
	}
	if _, err := store.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		return err
	}
	server := &scSyncPeer{r: store}
	request := func(c string, rule string) []xfer.Card {
		var cards []xfer.Card
		if c != "" {
			cards = append(cards, &xfer.PragmaCard{Name: "horizon", Values: []string{c, rule}})
		}
		return append(cards, &xfer.CloneCard{Version: 3, SeqNo: 1})
	}
	for name, cards := range map[string][]xfer.Card{
		"dense":    request("", ""),
		"ancestor": request(hzUUID(t, g.f.r, g.c2), "descendants"),
		"unknown":  request(strings.Repeat("ab", 20), "descendants"),
	} {
		resp, err := server.Exchange(context.Background(), &xfer.Message{Cards: cards})
		if err != nil {
			return err
		}
		if err := scSyncRefused(resp, scSyncOriginRefusal); err != nil {
			return expectf("%s: %v", name, err)
		}
		if err := scSyncRefusedWithCodes(resp); err != nil {
			return expectf("%s: %v", name, err)
		}
	}
	if _, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "dense.fossil"),
		server, sync.CloneOpts{}); err == nil || !strings.Contains(err.Error(), scSyncOriginRefusal) {
		return expectf("a dense clone of a horizon store: %v, want the refusal", err)
	}
	if err := scSyncFossilRefused(t, store); err != nil {
		return err
	}
	for _, sc := range []*cone.Cone{
		g.c,
		hzStoreCone(t, hzUUID(t, g.f.r, g.c3), cone.RuleLeaves, cone.DepthContent),
		hzStoreCone(t, hzUUID(t, g.f.r, g.c4), cone.RuleDescendants, cone.DepthContent),
	} {
		r, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "relay.fossil"),
			server, sync.CloneOpts{StoreCone: sc})
		if err != nil {
			return expectf("a covered horizon clone of a horizon store: %v", err)
		}
		r.Close()
	}
	// A store narrowed by paths alone holds no horizon-only request, which
	// keeps every path.
	_, paths, _, err := hzServeClone(t, g, scCone(t, []string{"src"}, nil), sync.HandleOpts{})
	if err != nil {
		return err
	}
	resp, err := (&scSyncPeer{r: paths}).Exchange(context.Background(),
		&xfer.Message{Cards: request(hzUUID(t, g.f.r, g.c3), "descendants")})
	if err != nil {
		return err
	}
	if err := scSyncRefused(resp, scSyncOriginRefusal); err != nil {
		return expectf("a path store asked for a horizon-only clone: %v", err)
	}
	return nil
}

// hzClusterPeer adds a cluster's file card to the first reply of each
// exchange it is armed for, as an origin ignoring §6.2's SHOULD NOT would.
type hzClusterPeer struct {
	*scSyncPeer
	uuid    string
	cluster []byte
	armed   bool
}

func (p *hzClusterPeer) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp, err := p.scSyncPeer.Exchange(ctx, req)
	if err != nil || !p.armed {
		return resp, err
	}
	p.armed = false
	resp.Cards = append(resp.Cards, &xfer.FileCard{UUID: p.uuid, Content: p.cluster})
	return resp, nil
}

// Against an origin that serves the horizon a store asks for no cluster
// catalog, and a cluster such an origin sends anyway is not held, so it
// mints no phantom for a name only the cluster gives (§4.5).
func hzServeNoChase(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer, r, _, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		return err
	}
	only := strings.Repeat("cd", 20)
	cluster, err := (&deck.Deck{Type: deck.Cluster, M: []string{only}}).Marshal()
	if err != nil {
		return err
	}
	uuid := hash.SHA1(cluster)
	injecting := &hzClusterPeer{scSyncPeer: peer, uuid: uuid, cluster: cluster}
	for range 2 {
		injecting.armed = true
		pulled, err := sync.Sync(context.Background(), r, injecting, sync.SyncOpts{Pull: true})
		if err != nil {
			return err
		}
		if pulled.HorizonServed == 0 {
			return expectf("fixture: the pull was not served at the horizon")
		}
	}
	if hzReqClusters(peer.requests) {
		return expectf("a store served at its horizon asked for the cluster catalog")
	}
	for _, name := range []string{uuid, only} {
		if _, known := blob.Exists(r.DB(), name); known {
			return expectf("the store names %s, which only a served cluster gives", name)
		}
	}
	return hzElided(r, g.below)
}
