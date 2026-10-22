package conformance

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The structural horizon over the wire (draft-fossil-horizon-00 §5.1, §5.3,
// §5.6, §6.1, §6.2, §6.2.2, §6.2.3, §6.2.5): go-libfossil's handler is the
// origin and its clone and pull the client, over hzSeed (trunk c1..c4 with
// C = c3, feat forked at c1 and merged with C by feat2). K is {c3, c4,
// feat2}; its boundary is c2 and feat1; c1 is named only by a tag.

func horizonStructuralCases() []Case {
	return []Case{horizonStructHoldings, horizonStructIgnorant, horizonStructLabels, horizonStructClassify,
		horizonStructReseed, horizonStructRequest, horizonStructRestrict, horizonStructLabelOrder,
		horizonStructSeedWire, horizonStructSeedRefused, horizonStructAdvertise, horizonStructServed,
		horizonStructFallback}
}

var (
	horizonStructHoldings    = Case{ID: "horizon.5.1.structural-holdings", Req: "HZ-5.1-1"}
	horizonStructIgnorant    = Case{ID: "horizon.5.1.ignorant-peer", Req: "HZ-5.1-2"}
	horizonStructLabels      = Case{ID: "horizon.5.3.labelled-elided", Req: "HZ-5.3-1"}
	horizonStructClassify    = Case{ID: "horizon.5.3.unlabelled-transit", Req: "HZ-5.3-1"}
	horizonStructReseed      = Case{ID: "horizon.5.6.reseed", Req: "HZ-5.6-1"}
	horizonStructRequest     = Case{ID: "horizon.6.1.structural-pragmas", Req: "HZ-6.1-1"}
	horizonStructRestrict    = Case{ID: "horizon.6.2.structural-restrict", Req: "HZ-6.2-1"}
	horizonStructLabelOrder  = Case{ID: "horizon.6.2.2.labels", Req: "HZ-6.2-4"}
	horizonStructSeedWire    = Case{ID: "horizon.6.2.3.seed-wire", Req: "HZ-6.2-5"}
	horizonStructSeedRefused = Case{ID: "horizon.6.2.3.seed-refused", Req: "HZ-5.2-4"}
	horizonStructAdvertise   = Case{ID: "horizon.6.2.3.advertise", Req: "HZ-6.2-6"}
	horizonStructServed      = Case{ID: "horizon.6.2.5.structural-served", Req: "HZ-6.2-8"}
	horizonStructFallback    = Case{ID: "horizon.6.3.structural-fallback", Req: "HZ-6.3-1"}
)

func TestHorizonStructural(t *testing.T) {
	check(t, horizonStructHoldings, func() error { return hzStructHoldings(t) })
	check(t, horizonStructIgnorant, func() error { return hzStructIgnorant(t) })
	check(t, horizonStructLabels, func() error { return hzStructLabels(t) })
	check(t, horizonStructClassify, func() error { return hzStructClassify(t) })
	check(t, horizonStructReseed, func() error { return hzStructReseed(t) })
	check(t, horizonStructRequest, func() error { return hzStructRequest(t) })
	check(t, horizonStructRestrict, func() error { return hzStructRestrict(t) })
	check(t, horizonStructLabelOrder, func() error { return hzStructLabelOrder(t) })
	check(t, horizonStructSeedWire, func() error { return hzStructSeedWire(t) })
	check(t, horizonStructSeedRefused, func() error { return hzStructSeedRefused(t) })
	check(t, horizonStructAdvertise, func() error { return hzStructAdvertise(t) })
	check(t, horizonStructServed, func() error { return hzStructServed(t) })
	check(t, horizonStructFallback, func() error { return hzStructFallback(t) })
}

// hzStruct is hzSeed's artifacts by name, for reading transcripts.
type hzStruct struct {
	s                            *hzSeed
	c1, c2, c3, c4, feat1, feat2 string
	controls                     map[string]bool // every tag the origin holds as a control artifact
}

func newHZStruct(t *testing.T) *hzStruct {
	t.Helper()
	s := newHZSeed(t)
	x := &hzStruct{s: s, controls: map[string]bool{}}
	for _, p := range []struct {
		dst *string
		rid libfossil.FslID
	}{{&x.c1, s.c1}, {&x.c2, s.c2}, {&x.c3, s.c3}, {&x.c4, s.c4}, {&x.feat1, s.feat1}, {&x.feat2, s.feat2}} {
		*p.dst = hzUUID(t, s.f.r, p.rid)
	}
	rows, err := s.f.r.DB().Query("SELECT uuid FROM blob JOIN event ON objid=rid WHERE type='g'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		rows.Scan(&u)
		x.controls[u] = true
	}
	if len(x.controls) != 3 {
		t.Fatalf("fixture: %d control artifacts, want 3", len(x.controls))
	}
	return x
}

// below is every check-in manifest outside K.
func (x *hzStruct) below() map[string]bool {
	return map[string]bool{x.c1: true, x.c2: true, x.feat1: true}
}

func (x *hzStruct) cone(t *testing.T) *cone.Cone {
	t.Helper()
	h, err := cone.NewHorizon(x.c3, cone.RuleDescendants, cone.DepthStructural)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cone.NewWithHorizon(nil, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// clone clones the origin at C's structural horizon through peer.
func (x *hzStruct) clone(t *testing.T, peer sync.Transport) (*repo.Repo, *sync.CloneResult, error) {
	t.Helper()
	r, result, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "structural.fossil"),
		peer, sync.CloneOpts{StoreCone: x.cone(t), PullPhantoms: true})
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(func() { r.Close() })
	return r, result, nil
}

func (x *hzStruct) peer() *scSyncPeer { return &scSyncPeer{r: x.s.f.r} }

// hzStructDepth is the depth of r's stored horizon.
func hzStructDepth(r *repo.Repo) (cone.HorizonDepth, error) {
	c, err := content.StoreCone(r.DB())
	if err != nil || c == nil {
		return 0, expectf("store cone %v, %v", c, err)
	}
	h, ok := c.Horizon()
	if !ok {
		return 0, expectf("store cone has no horizon")
	}
	return h.Depth(), nil
}

// hzStructHoldings: a structural clone holds K's manifests, every control
// artifact and the seed outside the artifact store, elided phantoms for the
// manifests below the horizon, no transit phantom, and derives K densely.
func hzStructHoldings(t *testing.T) error {
	x := newHZStruct(t)
	r, result, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	if result.HorizonFallback || result.StructuralServed == 0 {
		return expectf("clone fell back %v, served structural in %d of %d rounds",
			result.HorizonFallback, result.StructuralServed, result.Rounds)
	}
	if d, err := hzStructDepth(r); err != nil || d != cone.DepthStructural {
		return expectf("stored depth %v (%v), want structural", d, err)
	}
	if err := hzHeld(r, map[string]bool{x.c3: true, x.c4: true, x.feat2: true}, true); err != nil {
		return err
	}
	if err := hzHeld(r, x.controls, true); err != nil {
		return err
	}
	if err := hzHeld(r, x.below(), false); err != nil {
		return err
	}
	held, err := seed.Held(r.DB())
	if err != nil || held == nil {
		return expectf("no seed held (%v)", err)
	}
	var n int
	r.DB().QueryRow("SELECT count(*) FROM blob WHERE uuid=?", hash.SHA3(held)).Scan(&n)
	if n != 0 {
		return expectf("the seed is in the artifact store")
	}
	if err := hzElided(r, x.below()); err != nil {
		return err
	}
	return x.s.derivesDense(t, r)
}

// hzStructIgnorant: against a peer that ignores the horizon pragmas a
// structural clone never enters the structural horizon: it falls back to the
// content horizon, says so, and holds every manifest and no seed.
func hzStructIgnorant(t *testing.T) error {
	x := newHZStruct(t)
	peer := x.peer()
	peer.ignore = true
	r, result, err := x.clone(t, peer)
	if err != nil {
		return err
	}
	if !result.HorizonFallback || result.StructuralServed != 0 {
		return expectf("fallback %v, structural rounds %d; want a reported fallback",
			result.HorizonFallback, result.StructuralServed)
	}
	if d, err := hzStructDepth(r); err != nil || d != cone.DepthContent {
		return expectf("stored depth %v (%v), want content", d, err)
	}
	if held, _ := seed.Held(r.DB()); held != nil {
		return expectf("a seed is held after the fallback")
	}
	return hzHeld(r, x.below(), true)
}

// hzStructLabels: the boundary hashes are labelled, stored, and classified
// elided; they are never requested.
func hzStructLabels(t *testing.T) error {
	x := newHZStruct(t)
	peer := x.peer()
	r, _, err := x.clone(t, peer)
	if err != nil {
		return err
	}
	labels, err := content.BelowLabels(r.DB())
	if err != nil {
		return err
	}
	if err := scSameList("labels", scSorted(labels), scSorted(map[string]bool{x.c2: true, x.feat1: true})); err != nil {
		return err
	}
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	return hzNoGimme(peer.requests, x.below())
}

// hzStructClassify: an unlabelled unheld parent of a held check-in is a
// transit phantom, requested; once labelled it is elided.
func hzStructClassify(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	if _, err := r.DB().Exec("DELETE FROM horizonbelow WHERE uuid=?", x.c2); err != nil {
		return err
	}
	if got := hzClassOf(t, r, x.c2); got != content.Transit {
		return expectf("unlabelled c2 is %v, want transit", got)
	}
	peer := x.peer()
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	if scSyncNoGimme(peer.requests, x.c2) == nil {
		return expectf("the unlabelled parent was not requested")
	}
	if got := hzClassOf(t, r, x.c2); got != content.Elided {
		return expectf("relabelled c2 is %v, want elided", got)
	}
	return hzHeld(r, map[string]bool{x.c2: true}, false)
}

func hzClassOf(t *testing.T, r *repo.Repo, uuid string) content.Absence {
	t.Helper()
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	var rid libfossil.FslID
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	return a.Class(rid)
}

// hzStructReseed: every pull offers the held seed. Over unchanged tag state
// the origin sends none; after a tag below the horizon changes the state in
// force at C, it sends the new seed, and the clone derives densely again.
func hzStructReseed(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	first, _ := seed.Held(r.DB())
	peer := x.peer()
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	for i, req := range peer.requests {
		if got := hzPragmas(req, "horizon-seed"); len(got) != 1 || strings.Join(got[0], " ") != hash.SHA3(first) {
			return expectf("request %d offers %q, want the held seed", i, got)
		}
	}
	for i, reply := range peer.replies {
		if got := hzPragmas(reply, "horizon-seed"); len(got) != 0 {
			return expectf("reply %d re-sent an unchanged seed: %q", i, got)
		}
	}
	x.s.tag(t, x.s.c1, "hue", tag.TagPropagating, "blue")
	peer = x.peer()
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	now, _ := seed.Held(r.DB())
	if string(now) == string(first) {
		return expectf("the re-seeded pull kept the old seed")
	}
	if !strings.Contains(string(now), "T *hue "+x.c3+" blue\n") {
		return expectf("the new seed lacks the moved tag:\n%s", now)
	}
	return x.s.derivesDense(t, r)
}

// hzStructRequest: a structural store's every request carries horizon,
// horizon-structural and horizon-seed, naming - until a seed is held.
func hzStructRequest(t *testing.T) error {
	x := newHZStruct(t)
	peer := x.peer()
	r, _, err := x.clone(t, peer)
	if err != nil {
		return err
	}
	held, _ := seed.Held(r.DB())
	for i, req := range peer.requests {
		if got := hzPragmas(req, "horizon"); len(got) != 1 || strings.Join(got[0], " ") != x.c3+" descendants" {
			return expectf("request %d horizon %q", i, got)
		}
		if got := hzPragmas(req, "horizon-structural"); len(got) != 1 || len(got[0]) != 0 {
			return expectf("request %d horizon-structural %q", i, got)
		}
		want := hash.SHA3(held)
		if i == 0 {
			want = "-"
		}
		if got := hzPragmas(req, "horizon-seed"); len(got) != 1 || strings.Join(got[0], " ") != want {
			return expectf("request %d horizon-seed %q, want %s", i, got, want)
		}
	}
	return nil
}

// hzStructRestrict: the origin sends no manifest below the horizon and
// announces none, sends every control artifact, and sends the seed.
func hzStructRestrict(t *testing.T) error {
	x := newHZStruct(t)
	peer := x.peer()
	r, _, err := x.clone(t, peer)
	if err != nil {
		return err
	}
	if err := hzCrossed(peer.replies, x.below()); err != nil {
		return err
	}
	sent := hzSent(peer.replies)
	for u := range x.controls {
		if _, ok := sent[u]; !ok {
			return expectf("the origin did not send control artifact %s", u)
		}
	}
	held, _ := seed.Held(r.DB())
	if _, ok := sent[hash.SHA3(held)]; !ok {
		return expectf("the origin did not send the seed")
	}
	pull := x.peer()
	c5 := hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "c5", []libfossil.FslID{x.s.c4}, nil))
	feat3 := hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "feat3", []libfossil.FslID{x.s.feat1}, nil))
	if _, err := sync.Sync(context.Background(), r, pull, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	if err := hzHeld(r, map[string]bool{c5: true}, true); err != nil {
		return err
	}
	return hzCrossed(pull.replies, hzUnion(x.below(), map[string]bool{feat3: true}))
}

// hzStructLabelOrder: each boundary hash is labelled no later than the reply
// carrying the first manifest that names it.
func hzStructLabelOrder(t *testing.T) error {
	x := newHZStruct(t)
	peer := x.peer()
	if _, _, err := x.clone(t, peer); err != nil {
		return err
	}
	labelled := map[string]bool{}
	for i, reply := range peer.replies {
		for _, v := range hzPragmas(reply, "horizon-below") {
			labelled[strings.Join(v, " ")] = true
		}
		sent := hzSent([]*xfer.Message{reply})
		for child, parent := range map[string]string{x.c3: x.c2, x.feat2: x.feat1} {
			if _, ok := sent[child]; ok && !labelled[parent] {
				return expectf("reply %d carries %.10s before labelling its parent", i, child)
			}
		}
	}
	if !labelled[x.c2] || !labelled[x.feat1] || len(labelled) != 2 {
		return expectf("labelled %v, want c2 and feat1", scSorted(labelled))
	}
	return nil
}

// hzStructSeedWire: the seed pragma precedes its card; the clone holds the
// card as the seed only when its bytes hash to the announced name and parse
// as a control artifact, drops one that fails, and never stores, announces
// or sends the seed.
func hzStructSeedWire(t *testing.T) error {
	x := newHZStruct(t)
	peer := x.peer()
	r, _, err := x.clone(t, peer)
	if err != nil {
		return err
	}
	name := hash.SHA3(mustHeld(t, r))
	for i, reply := range peer.replies {
		announced := -1
		for j, card := range reply.Cards {
			if p, ok := card.(*xfer.PragmaCard); ok && p.Name == "horizon-seed" {
				announced = j
			}
			if f, ok := card.(*xfer.CFileCard); ok && f.UUID == name && (announced < 0 || announced > j) {
				return expectf("reply %d sends the seed before announcing it", i)
			}
			if f, ok := card.(*xfer.FileCard); ok && f.UUID == name && (announced < 0 || announced > j) {
				return expectf("reply %d sends the seed before announcing it", i)
			}
		}
	}
	pushed := x.peer()
	if _, err := sync.Sync(context.Background(), r, pushed, sync.SyncOpts{Push: true, Pull: true}); err != nil {
		return err
	}
	for i, req := range pushed.requests {
		for _, card := range req.Cards {
			switch c := card.(type) {
			case *xfer.IGotCard:
				if c.UUID == name {
					return expectf("request %d announces the seed", i)
				}
			case *xfer.FileCard, *xfer.CFileCard:
				if _, ok := scSyncPayloads(&xfer.Message{Cards: []xfer.Card{c}})[name]; ok {
					return expectf("request %d sends the seed", i)
				}
			}
		}
	}
	// A reply announcing a name its card does not hash to, and one naming a
	// manifest as the seed: neither card is held, neither is stored.
	for _, rewrite := range []func(*xfer.Message){hzSeedCorrupt, hzSeedDivert(x.c4)} {
		bad := &hzRewritingPeer{inner: x.peer(), rewrite: rewrite}
		r, result, err := x.clone(t, bad)
		if err != nil {
			return err
		}
		if held, _ := seed.Held(r.DB()); held != nil {
			return expectf("a failing seed card was held")
		}
		if !result.HorizonFallback {
			return expectf("a clone holding no seed did not fall back")
		}
	}
	return nil
}

func mustHeld(t *testing.T, r *repo.Repo) []byte {
	t.Helper()
	held, err := seed.Held(r.DB())
	if err != nil || held == nil {
		t.Fatalf("no seed held (%v)", err)
	}
	return held
}

// hzSeedCorrupt flips a byte of the seed card a reply announces.
func hzSeedCorrupt(m *xfer.Message) {
	name := ""
	for _, card := range m.Cards {
		if p, ok := card.(*xfer.PragmaCard); ok && p.Name == "horizon-seed" && len(p.Values) == 1 {
			name = p.Values[0]
		}
		switch c := card.(type) {
		case *xfer.FileCard:
			if c.UUID == name {
				c.Content = append([]byte("X"), c.Content...)
			}
		case *xfer.CFileCard:
			if c.UUID == name {
				c.Content = append([]byte("X"), c.Content...)
			}
		}
	}
}

// hzSeedDivert renames a reply's seed announcement to target, a manifest the
// reply also sends, as an origin diverting an artifact would.
func hzSeedDivert(target string) func(*xfer.Message) {
	return func(m *xfer.Message) {
		for _, card := range m.Cards {
			if p, ok := card.(*xfer.PragmaCard); ok && p.Name == "horizon-seed" && len(p.Values) == 1 {
				p.Values[0] = target
			}
		}
	}
}

// hzRewritingPeer passes each reply of inner through rewrite.
type hzRewritingPeer struct {
	inner   sync.Transport
	rewrite func(*xfer.Message)
}

func (p *hzRewritingPeer) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp, err := p.inner.Exchange(ctx, req)
	if err == nil {
		p.rewrite(resp)
	}
	return resp, err
}

// hzStructSeedRefused: an origin refuses a seed offered to it, storing no
// file the request names as its seed.
func hzStructSeedRefused(t *testing.T) error {
	x := newHZStruct(t)
	name, data, err := x.s.mint("origin")
	if err != nil {
		return err
	}
	var pc string
	x.s.f.r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc)
	req := &xfer.Message{Cards: append(slices.Clone(conePragmaCards(x)), &xfer.PragmaCard{Name: "horizon-seed", Values: []string{name}},
		&xfer.PushCard{ServerCode: "x", ProjectCode: pc}, &xfer.FileCard{UUID: name, Content: data})}
	if _, err := sync.HandleSync(context.Background(), x.s.f.r, req); err != nil {
		return err
	}
	var n int
	x.s.f.r.DB().QueryRow("SELECT count(*) FROM blob WHERE uuid=?", name).Scan(&n)
	if n != 0 {
		return expectf("the origin stored a seed offered to it")
	}
	return nil
}

func conePragmaCards(x *hzStruct) []xfer.Card {
	return []xfer.Card{
		&xfer.PragmaCard{Name: "client-version", Values: []string{"22800", "20260101", "000000"}},
		&xfer.PragmaCard{Name: "horizon", Values: []string{x.c3, "descendants"}},
		&xfer.PragmaCard{Name: "horizon-structural"},
	}
}

// hzStructAdvertise: the origin advertises the structural level only with a
// seed in the reply or the client's offered seed current. An offered current
// seed is answered structural with no seed; a horizon it cannot place is
// answered content, with no seed and no labels.
func hzStructAdvertise(t *testing.T) error {
	x := newHZStruct(t)
	name, _, err := x.s.mint("origin")
	if err != nil {
		return err
	}
	var pc string
	x.s.f.r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc)
	ask := func(c, offered string) (*xfer.Message, error) {
		cards := []xfer.Card{
			&xfer.PragmaCard{Name: "client-version", Values: []string{"22800", "20260101", "000000"}},
			&xfer.PragmaCard{Name: "horizon", Values: []string{c, "descendants"}},
			&xfer.PragmaCard{Name: "horizon-structural"},
			&xfer.PragmaCard{Name: "horizon-seed", Values: []string{offered}},
			&xfer.PullCard{ServerCode: "x", ProjectCode: pc},
		}
		resp, err := sync.HandleSync(context.Background(), x.s.f.r, &xfer.Message{Cards: cards})
		if err != nil {
			return nil, err
		}
		return scSyncWire(resp)
	}
	fresh, err := ask(x.c3, "-")
	if err != nil {
		return err
	}
	if got := hzPragmas(fresh, "horizon-served"); len(got) != 1 || got[0][0] != "structural" {
		return expectf("a seeded reply advertises %q", got)
	}
	if got := hzPragmas(fresh, "horizon-seed"); len(got) != 1 || got[0][0] != name {
		return expectf("a seeded reply announces %q, want %s", got, name)
	}
	current, err := ask(x.c3, name)
	if err != nil {
		return err
	}
	if got := hzPragmas(current, "horizon-served"); len(got) != 1 || got[0][0] != "structural" {
		return expectf("a reply to a current seed advertises %q", got)
	}
	if got := hzPragmas(current, "horizon-seed"); len(got) != 0 {
		return expectf("a reply to a current seed re-sends it: %q", got)
	}
	unknown, err := ask(strings.Repeat("ab", 20), "-")
	if err != nil {
		return err
	}
	if got := hzPragmas(unknown, "horizon-served"); len(got) != 1 || got[0][0] != "content" {
		return expectf("an unplaceable horizon advertises %q", got)
	}
	if n := len(hzPragmas(unknown, "horizon-seed")) + len(hzPragmas(unknown, "horizon-below")); n != 0 {
		return expectf("an unplaceable horizon sent %d seed or label pragmas", n)
	}
	return nil
}

// hzStructServed: every reply to a structural clone says horizon-served
// structural and the client counts them.
func hzStructServed(t *testing.T) error {
	x := newHZStruct(t)
	peer := x.peer()
	_, result, err := x.clone(t, peer)
	if err != nil {
		return err
	}
	if result.StructuralServed != result.Rounds || result.HorizonServed != result.Rounds {
		return expectf("structural %d, served %d of %d rounds", result.StructuralServed, result.HorizonServed, result.Rounds)
	}
	for i, reply := range peer.replies {
		if got := hzPragmas(reply, "horizon-served"); len(got) != 1 || strings.Join(got[0], " ") != "structural" {
			return expectf("reply %d advertises %q", i, got)
		}
	}
	return nil
}

// hzStructFallback: a structural store pulling from a peer that serves only
// the content horizon falls back to it for good and reports it: the store
// cone becomes the content horizon, the seed is discarded, and the manifests
// below the horizon are fetched.
func hzStructFallback(t *testing.T) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	peer := x.peer()
	peer.ignore = true
	result, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if !result.HorizonFallback {
		return expectf("the pull did not report a fallback")
	}
	if d, err := hzStructDepth(r); err != nil || d != cone.DepthContent {
		return expectf("stored depth %v (%v), want content", d, err)
	}
	if held, _ := seed.Held(r.DB()); held != nil {
		return expectf("the seed survived the fallback")
	}
	return hzHeld(r, x.below(), true)
}
