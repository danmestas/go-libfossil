package conformance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// draft-fossil-horizon-00 (zit 3a2df62515) Section 3, 4, 5 and 6.2: a path
// cone elides sub-manifest nodes under either horizon, as the sparse store
// does. Of the trees the horizon keeps -- every check-in's under a content
// horizon, K's under a structural one -- only the nodes on paths into the
// cone are held; an origin withholds the rest, and a cursor is void once an
// appended tree keeps an older withheld node (§6.2.6).
var (
	horizonNodesStore      = Case{ID: "horizon.3.2.path-nodes", Req: "HZ-3.2-2"}
	horizonNodesHeld       = Case{ID: "horizon.4.1.path-nodes", Req: "HZ-4.1-1"}
	horizonNodesContent    = Case{ID: "horizon.4.1.path-nodes-every-age", Req: "HZ-4.1-2"}
	horizonNodesWholeTree  = Case{ID: "horizon.4.1.whole-tree-read", Req: "HZ-4.1-2"}
	horizonNodesDerived    = Case{ID: "horizon.4.3.derivation-reads-no-node", Req: "HZ-4.3-2"}
	horizonNodesStructural = Case{ID: "horizon.5.1.path-nodes", Req: "HZ-5.1-1"}
	horizonNodesServe      = Case{ID: "horizon.6.2.path-nodes-withheld", Req: "HZ-6.2-14"}
	horizonNodesWalk       = Case{ID: "horizon.6.2.slot-walk-kept", Req: "HZ-6.2-15"}
	horizonNodesRekept     = Case{ID: "horizon.6.2.6.node-rekept", Req: "HZ-6.2-11"}
)

func horizonNodesCases() []Case {
	return []Case{horizonNodesStore, horizonNodesHeld, horizonNodesContent, horizonNodesWholeTree,
		horizonNodesDerived, horizonNodesStructural, horizonNodesServe, horizonNodesWalk, horizonNodesRekept}
}

func TestHorizonNodes(t *testing.T) {
	check(t, horizonNodesStore, func() error { return hzNodesClone(t, cone.DepthContent) })
	check(t, horizonNodesHeld, func() error { return hzNodesClone(t, cone.DepthContent) })
	check(t, horizonNodesContent, func() error { return hzNodesClone(t, cone.DepthContent) })
	check(t, horizonNodesWholeTree, func() error { return hzNodesWholeTree(t) })
	check(t, horizonNodesDerived, func() error { return hzNodesDerived(t) })
	check(t, horizonNodesStructural, func() error { return hzNodesClone(t, cone.DepthStructural) })
	check(t, horizonNodesServe, func() error { return hzNodesServe(t) })
	check(t, horizonNodesWalk, func() error { return hzNodesWalk(t) })
	check(t, horizonNodesRekept, func() error { return hzNodesRekept(t) })
}

// hzNodesOrigin is a tree origin with three check-ins, c1 below the horizon
// at c2, each changing files inside src and outside it.
type hzNodesOrigin struct {
	f          *scRepo
	c1, c2, c3 libfossil.FslID
}

func newHZNodesOrigin(t *testing.T) *hzNodesOrigin {
	t.Helper()
	f := newSCSyncTreeRepo(t)
	files := scSyncTreeFiles()
	files["src/a"], files["zzz/deep/c"] = "inside 2\n", "deep 2\n"
	c2 := f.commit(t, files, f.c1)
	files["yyy/d"], files["src/shared"] = "y 3\n", "shared 3\n"
	c3 := f.commit(t, files, c2)
	return &hzNodesOrigin{f: f, c1: f.c1, c2: c2, c3: c3}
}

func (o *hzNodesOrigin) cone(t *testing.T, depth cone.HorizonDepth) *cone.Cone {
	t.Helper()
	return hzStoreCone(t, hzUUID(t, o.f.r, o.c2), cone.RuleDescendants, depth, "src")
}

// hzSlotNodes is the oracle of §4.1 and §6.2: every node reached from the
// trees of checkins through slots that may hold a path sc covers, each tree
// walked on its own from its manifest, a last slot bounded by its parent's.
func hzSlotNodes(t *testing.T, r *repo.Repo, sc *cone.Cone, checkins ...libfossil.FslID) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var walk func(cards []deck.SubmanifestCard, upper string)
	walk = func(cards []deck.SubmanifestCard, upper string) {
		for i, s := range cards {
			limit := upper
			if i+1 < len(cards) {
				limit = cards[i+1].Name
			}
			if !sc.MayCover(s.Name, limit) {
				continue
			}
			out[s.UUID] = true
			if rid, ok := content.AvailableByUUID(r.DB(), s.UUID); ok {
				walk(hzDeckOf(t, r, rid).S, limit)
			}
		}
	}
	for _, c := range checkins {
		walk(hzDeckOf(t, r, c).S, "")
	}
	return out
}

// hzNodes is every sub-manifest node r holds.
func hzNodes(t *testing.T, r *repo.Repo) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for uuid, d := range scSyncTreeOrigin(t, r) {
		if d != nil && d.Type == deck.Submanifest {
			out[uuid] = true
		}
	}
	return out
}

func hzMinusSet(all, drop map[string]bool) map[string]bool {
	out := map[string]bool{}
	for uuid := range all {
		if !drop[uuid] {
			out[uuid] = true
		}
	}
	return out
}

func hzIntersect(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for uuid := range a {
		if b[uuid] {
			out[uuid] = true
		}
	}
	return out
}

// A clone at either horizon with path members is sent exactly the nodes on
// paths into the cone of the trees the horizon keeps (every check-in's at
// the content depth, of every age; K's at the structural one), is sent and
// announced none of the others, holds each kept tree whole within the cone,
// classifies every withheld node it can name as elided, holds no transit
// phantom, and verifies clean.
func hzNodesClone(t *testing.T, depth cone.HorizonDepth) error {
	o := newHZNodesOrigin(t)
	sc := o.cone(t, depth)
	trees := []libfossil.FslID{o.c1, o.c2, o.c3}
	if depth == cone.DepthStructural {
		trees = trees[1:]
	}
	want := hzSlotNodes(t, o.f.r, sc, trees...)
	all := hzNodes(t, o.f.r)
	withheld := hzMinusSet(all, want)
	if len(want) == 0 || len(withheld) == 0 {
		return expectf("fixture: %d nodes kept, %d withheld", len(want), len(withheld))
	}
	if depth == cone.DepthContent && len(hzMinusSet(hzSlotNodes(t, o.f.r, sc, o.c1), hzSlotNodes(t, o.f.r, sc, o.c2, o.c3))) == 0 {
		return expectf("fixture: c1, below the horizon, has no in-cone node of its own")
	}
	peer := &scSyncPeer{r: o.f.r}
	r, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "nodes.fossil"),
		peer, sync.CloneOpts{StoreCone: sc})
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	if err := hzSameNames("nodes sent", hzIntersect(scSetOf(hzSent(peer.replies)), all), want); err != nil {
		return err
	}
	if err := hzCrossed(peer.replies, withheld); err != nil {
		return err
	}
	if err := hzHeld(r, want, true); err != nil {
		return err
	}
	return hzNodesClean(r, withheld)
}

// scSetOf is the names of a sent map.
func scSetOf(sent map[string]string) map[string]bool {
	out := make(map[string]bool, len(sent))
	for uuid := range sent {
		out[uuid] = true
	}
	return out
}

// hzNodesClean checks r holds no transit phantom, classifies each of the
// withheld nodes it has a row for as elided, and verifies clean.
func hzNodesClean(r *repo.Repo, withheld map[string]bool) error {
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		return err
	}
	if n := a.Count(content.Transit); n != 0 {
		return expectf("%d transit phantoms, want none", n)
	}
	named := 0
	for uuid := range withheld {
		rid, err := hzRID(r, uuid)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		named++
		if got := a.Class(rid); got != content.Elided {
			return expectf("withheld node %s is %v, want elided", uuid, got)
		}
	}
	if named == 0 {
		return expectf("fixture: the clone names no withheld node")
	}
	report, err := verify.Verify(r)
	if err != nil {
		return err
	}
	if !report.OK() || report.Phantoms != 0 {
		return expectf("verify ok=%v phantoms=%d issues=%v", report.OK(), report.Phantoms, report.Issues)
	}
	return nil
}

// An operation that needs an elided node, such as a commit's whole tree,
// obtains it as the sparse store does (sparse-cone §5.3): the read refuses
// naming elision, not damage, and the exact fetch of the roots it names,
// which carries no cone pragmas, brings the nodes and leaves the cone as it
// was.
func hzNodesWholeTree(t *testing.T) error {
	o := newHZNodesOrigin(t)
	sc := o.cone(t, cone.DepthContent)
	peer := &scSyncPeer{r: o.f.r}
	r, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "nodes.fossil"),
		peer, sync.CloneOpts{StoreCone: sc})
	if err != nil {
		return err
	}
	defer r.Close()
	tip, err := hzRID(r, hzUUID(t, o.f.r, o.c3))
	if err != nil {
		return err
	}
	ctx := context.Background()
	var refused *manifest.RequiredContentError
	err = manifest.RequireParentContent(ctx, r.DB(), []libfossil.FslID{tip})
	if !errors.As(err, &refused) || refused.Elided == 0 {
		return expectf("a whole-tree read over withheld nodes was not refused as elision: %v", err)
	}
	if _, err := sync.FetchRequiredArtifacts(ctx, r, peer, refused.Roots, sync.SyncOpts{}); err != nil {
		return err
	}
	if err := manifest.RequireParentContent(ctx, r.DB(), []libfossil.FslID{tip}); err != nil {
		return expectf("the read still refuses after the fetch: %v", err)
	}
	got, err := content.StoreCone(r.DB())
	if err != nil || got == nil || !got.Equal(sc) {
		return expectf("the fetch changed the store cone: %v, %v", got, err)
	}
	return nil
}

// The derivation reads no node (§4.3): a clone at a content horizon with
// path members derives what a dense clone derives.
func hzNodesDerived(t *testing.T) error {
	o := newHZNodesOrigin(t)
	derived := map[bool]string{}
	for _, horizon := range []bool{false, true} {
		var sc *cone.Cone
		if horizon {
			sc = o.cone(t, cone.DepthContent)
		}
		r, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), fmt.Sprintf("d%v.fossil", horizon)),
			&scSyncPeer{r: o.f.r}, sync.CloneOpts{StoreCone: sc})
		if err != nil {
			return err
		}
		defer r.Close()
		var out string
		if err := r.DB().QueryRow(`SELECT
			coalesce((SELECT group_concat(x, ';') FROM (SELECT p.uuid||':'||c.uuid||':'||isprim AS x FROM plink
				JOIN blob p ON p.rid=pid JOIN blob c ON c.rid=cid ORDER BY 1)), '') || '|' ||
			coalesce((SELECT group_concat(x, ';') FROM (SELECT b.uuid AS x FROM leaf JOIN blob b ON b.rid=leaf.rid ORDER BY 1)), '') || '|' ||
			coalesce((SELECT group_concat(x, ';') FROM (SELECT t.tagname||':'||b.uuid||':'||tagtype||':'||coalesce(value,'') AS x
				FROM tagxref x JOIN tag t ON t.tagid=x.tagid JOIN blob b ON b.rid=x.rid ORDER BY 1)), '') || '|' ||
			coalesce((SELECT group_concat(x, ';') FROM (SELECT b.uuid||':'||type||':'||mtime AS x FROM event
				JOIN blob b ON b.rid=objid ORDER BY 1)), '')`).Scan(&out); err != nil {
			return err
		}
		derived[horizon] = out
	}
	if parts := strings.Split(derived[false], "|"); len(parts) != 4 || parts[0] == "" || parts[3] == "" {
		return expectf("fixture: the dense clone derived %q", derived[false])
	}
	if derived[true] != derived[false] {
		return expectf("the horizon clone derived\n%s\nthe dense one\n%s", derived[true], derived[false])
	}
	return nil
}

// A pull at either horizon with path members is announced only the kept
// nodes, and a gimme for a withheld node goes unanswered; a request without
// path members, horizon or not, is answered every node.
func hzNodesServe(t *testing.T) error {
	o := newHZNodesOrigin(t)
	all := hzNodes(t, o.f.r)
	paths := &xfer.PragmaCard{Name: "cone", Values: []string{"src"}}
	// A peer that does not say it reads sub-manifests is refused every node
	// (draft-fossil-submanifests-00 §7.2), which would hide the withholding.
	reads := &xfer.PragmaCard{Name: "send-submanifests"}
	for _, depth := range []cone.HorizonDepth{cone.DepthContent, cone.DepthStructural} {
		sc := o.cone(t, depth)
		trees := []libfossil.FslID{o.c1, o.c2, o.c3}
		var extra []xfer.Card
		if depth == cone.DepthStructural {
			trees = trees[1:]
			extra = []xfer.Card{&xfer.PragmaCard{Name: "horizon-structural"},
				&xfer.PragmaCard{Name: "horizon-seed", Values: []string{"-"}}}
		}
		want := hzSlotNodes(t, o.f.r, sc, trees...)
		reply, err := hzAsk(t, o.f.r, sc, true, false, hzNoCursor, append([]xfer.Card{paths}, extra...)...)
		if err != nil {
			return err
		}
		if err := hzSameNames(fmt.Sprintf("nodes announced at depth %v", depth),
			hzIntersect(hzIGots(reply), all), want); err != nil {
			return err
		}
		withheld := scSorted(hzMinusSet(all, want))[0]
		gimme := &xfer.GimmeCard{UUID: withheld}
		for _, ask := range []struct {
			sc    *cone.Cone
			cards []xfer.Card
			sent  bool
		}{
			{sc, append([]xfer.Card{reads, paths, gimme}, extra...), false},
			{sc, append([]xfer.Card{reads, gimme}, extra...), true},
			{nil, []xfer.Card{reads, gimme}, true},
		} {
			reply, err := hzAsk(t, o.f.r, ask.sc, true, false, hzNoCursor, ask.cards...)
			if err != nil {
				return err
			}
			if _, got := hzSent([]*xfer.Message{reply})[withheld]; got != ask.sent {
				return expectf("depth %v, %d cards: gimme for withheld node answered=%v, want %v",
					depth, len(ask.cards), got, ask.sent)
			}
		}
	}
	return nil
}

// The origin keeps its walk with its classification (§6.2): a later request
// walks only the tree appended since, and the kept walk equals a fresh one.
func hzNodesWalk(t *testing.T) error {
	o := newHZNodesOrigin(t)
	sc := o.cone(t, cone.DepthContent)
	ctx := context.Background()
	memo := content.NewMemo()
	walk := func(m *content.Memo) (*content.SlotKeep, error) {
		v, err := m.View(ctx, o.f.r.DB())
		if err != nil {
			return nil, err
		}
		refs, err := v.References(ctx, sc)
		if err != nil {
			return nil, err
		}
		k, err := v.SlotKeep(ctx, refs, sc, nil)
		if err == nil {
			v.Publish()
		}
		return k, err
	}
	first, err := walk(memo)
	if err != nil {
		return err
	}
	if first.Visits() == 0 {
		return expectf("the first walk visited nothing")
	}
	files := scSyncTreeFiles()
	files["src/a"], files["zzz/b"] = "inside 4\n", "outside 4\n"
	c4 := o.f.commit(t, files, o.c3)
	next, err := walk(memo)
	if err != nil {
		return err
	}
	fresh, err := walk(content.NewMemo())
	if err != nil {
		return err
	}
	if !next.Equal(fresh) {
		return expectf("the kept walk differs from a fresh one")
	}
	own := len(hzSlotNodes(t, o.f.r, sc, c4))
	if next.Visits() == 0 || next.Visits() > own {
		return expectf("the extension visited %d nodes; the new tree has %d on paths into the cone", next.Visits(), own)
	}
	if again, err := walk(memo); err != nil || again.Visits() != 0 {
		return expectf("an unchanged store was walked again: %d visits, %v", again.Visits(), err)
	}
	return nil
}

// draft-fossil-horizon-00 §6.2.6 and zit's walkSlots (sparse.zig at
// 3a2df62515): n_lib's last slot, q/a, takes its bound from its parent. c1
// names r/x after n_lib, so the slot misses src and l_q is withheld; c2
// names n_lib last, the slot runs to the end, and l_q, an older node, is
// kept. A cursor handed out before c2 arrived is then void, once.
func hzNodesRekept(t *testing.T) error {
	o := newSCSyncTreeRepo(t)
	b := &hzTreeBuilder{t: t, when: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	lLib, lQ, lR := b.leaf("lib/a"), b.leaf("q/a"), b.leaf("r/x")
	nLib := b.node(deck.Submanifest, "", "lib/a", lLib, "q/a", lQ)
	c1 := b.node(deck.Checkin, "", "lib/a", nLib, "r/x", lR)
	if err := b.push(o.r); err != nil {
		return err
	}
	sc := hzStoreCone(t, c1, cone.RuleDescendants, cone.DepthContent, "lib", "src")
	paths := []xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{"lib"}},
		&xfer.PragmaCard{Name: "cone", Values: []string{"src"}}}
	before, err := hzAsk(t, o.r, sc, true, false, hzNoCursor, paths...)
	if err != nil {
		return err
	}
	if hzIGots(before)[lQ] || !hzIGots(before)[nLib] {
		return expectf("under c1: l_q announced %v, n_lib %v; want false, true", hzIGots(before)[lQ], hzIGots(before)[nLib])
	}
	old, err := hzCursorOf(before)
	if err != nil {
		return err
	}
	b.node(deck.Checkin, c1, "lib/a", nLib)
	if err := b.push(o.r); err != nil {
		return err
	}
	full, err := hzAsk(t, o.r, sc, true, false, hzNoCursor, paths...)
	if err != nil {
		return err
	}
	if !hzIGots(full)[lQ] {
		return expectf("under c2, l_q is not announced")
	}
	voided, err := hzAsk(t, o.r, sc, true, false, old, paths...)
	if err != nil {
		return err
	}
	if err := hzSameNames(fmt.Sprintf("announced under the void cursor %d", old), hzIGots(voided), hzIGots(full)); err != nil {
		return err
	}
	next, err := hzCursorOf(voided)
	if err != nil {
		return err
	}
	after, err := hzAsk(t, o.r, sc, true, false, next, paths...)
	if err != nil {
		return err
	}
	if got := hzIGots(after); len(got) != 0 {
		return expectf("the cursor %d past the rekept node was not honoured: %d announced", next, len(got))
	}
	return nil
}

// hzTreeBuilder makes hand-built trees and pushes them, files first.
type hzTreeBuilder struct {
	t       *testing.T
	when    time.Time
	pending []*xfer.FileCard
}

func (b *hzTreeBuilder) add(data []byte) string {
	uuid := hash.SHA3(data)
	b.pending = append(b.pending, &xfer.FileCard{UUID: uuid, Content: data})
	return uuid
}

func (b *hzTreeBuilder) leaf(names ...string) string {
	b.t.Helper()
	d := &deck.Deck{Type: deck.Submanifest}
	for _, name := range names {
		d.F = append(d.F, deck.FileCard{Name: name, UUID: b.add([]byte(name + " built\n"))})
	}
	return b.marshal(d)
}

func (b *hzTreeBuilder) node(kind deck.ArtifactType, parent string, pairs ...string) string {
	b.t.Helper()
	d := &deck.Deck{Type: kind}
	for i := 0; i < len(pairs); i += 2 {
		d.S = append(d.S, deck.SubmanifestCard{Name: pairs[i], UUID: pairs[i+1]})
	}
	if kind == deck.Checkin {
		b.when = b.when.Add(time.Hour)
		d.C, d.D, d.U = "built", b.when, deck.User("u")
		if parent != "" {
			d.P = []string{parent}
		}
	}
	return b.marshal(d)
}

func (b *hzTreeBuilder) marshal(d *deck.Deck) string {
	b.t.Helper()
	data, err := d.Marshal()
	if err != nil {
		b.t.Fatal(err)
	}
	return b.add(data)
}

// push delivers every pending artifact to origin in one push.
func (b *hzTreeBuilder) push(origin *repo.Repo) error {
	cards := make([]xfer.Card, 0, len(b.pending))
	for _, c := range b.pending {
		cards = append(cards, c)
	}
	b.pending = nil
	reply, err := hzAsk(b.t, origin, nil, false, true, hzNoCursor, cards...)
	if err != nil {
		return err
	}
	if errs := hzErrors(reply); len(errs) != 0 {
		return expectf("push: %q", errs)
	}
	return nil
}
