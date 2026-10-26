package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// An origin identifies a pushed seed by its own seed label, or by a
// horizon-seed T card under any label, on expanded content, and refuses the
// whole request with one error card (horizon §5.2.3). Each case pushes a seed, with no
// horizon-seed pragma naming it, beside an ordinary artifact, and checks
// that the reply is the refusal alone and that neither was stored.

func horizonSeedRefuseCases() []Case {
	return []Case{horizonSeedRefuseFull, horizonSeedRefuseDelta, horizonSeedRefuseLate, horizonPushCheckLate,
		horizonSeedShapeFull, horizonSeedShapeLate, horizonForeignOrdinary, horizonSeedLaterRequest}
}

var (
	horizonSeedRefuseFull   = Case{ID: "horizon.5.2.3.seed-label-full-text", Req: "HZ-5.2-4"}
	horizonSeedRefuseDelta  = Case{ID: "horizon.5.2.3.seed-label-delta", Req: "HZ-5.2-4"}
	horizonSeedRefuseLate   = Case{ID: "horizon.5.2.3.seed-label-late-delta", Req: "HZ-5.2-4"}
	horizonSeedShapeFull    = Case{ID: "horizon.5.2.3.seed-shape-foreign-label", Req: "HZ-5.2-10"}
	horizonSeedShapeLate    = Case{ID: "horizon.5.2.3.seed-shape-late-delta", Req: "HZ-5.2-10"}
	horizonForeignOrdinary  = Case{ID: "horizon.5.2.3.foreign-label-ordinary", Req: "HZ-5.2-10"}
	horizonSeedLaterRequest = Case{ID: "horizon.5.2.3.seed-base-later-request", Req: "HZ-5.2-9"}
)

const hzSeedRefused = "seed refused: a seed is private to the clone that holds it; it is never pushed"

func TestHorizonSeedRefuse(t *testing.T) {
	check(t, horizonSeedRefuseFull, func(t *testing.T) error { return hzSeedRefuse(t, hzOwnSeed, hzFullText) })
	check(t, horizonSeedRefuseDelta, func(t *testing.T) error {
		return hzSeedRefuse(t, hzOwnSeed, func(x *hzStruct, name string, data []byte) ([]xfer.Card, []string, error) {
			rid, ok := blob.Exists(x.s.f.r.DB(), x.c3)
			if !ok {
				return nil, nil, expectf("the origin does not hold c3")
			}
			base, err := content.Expand(x.s.f.r.DB(), rid)
			if err != nil {
				return nil, nil, err
			}
			d, err := delta.Create(base, data)
			if err != nil {
				return nil, nil, err
			}
			return []xfer.Card{&xfer.FileCard{UUID: name, DeltaSrc: x.c3, Content: d}}, nil, nil
		})
	})
	check(t, horizonPushCheckLate, func(t *testing.T) error { return hzPushCheckLate(t) })
	check(t, horizonSeedRefuseLate, func(t *testing.T) error { return hzSeedRefuse(t, hzOwnSeed, hzLateDelta) })
	check(t, horizonSeedShapeFull, func(t *testing.T) error { return hzSeedRefuse(t, hzForeignSeed(true), hzFullText) })
	check(t, horizonSeedShapeLate, func(t *testing.T) error { return hzSeedRefuse(t, hzForeignSeed(true), hzLateDelta) })
	check(t, horizonForeignOrdinary, func(t *testing.T) error { return hzForeignStored(t) })
	check(t, horizonSeedLaterRequest, func(t *testing.T) error { return hzSeedLaterRequest(t) })
}

// hzOwnSeed is the seed the origin mints, under its own label.
func hzOwnSeed(x *hzStruct) (string, []byte, error) { return x.s.mint(seedLoginGo) }

// hzForeignSeed is a control artifact on c3 under zit's seed label, carrying
// a horizon-seed T card when bound.
func hzForeignSeed(bound bool) func(x *hzStruct) (string, []byte, error) {
	return func(x *hzStruct) (string, []byte, error) {
		name := "hue"
		if bound {
			name = "horizon-seed"
		}
		d := &deck.Deck{Type: deck.Control, D: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), U: deck.User("zit-seed"),
			T: []deck.TagCard{{Type: deck.TagSingleton, Name: name, UUID: x.c3, Value: x.c3}}}
		data, err := d.Marshal()
		if err != nil {
			return "", nil, err
		}
		return hash.SHA1(data), data, nil
	}
}

// hzFullText sends the seed as full text.
func hzFullText(x *hzStruct, name string, data []byte) ([]xfer.Card, []string, error) {
	return []xfer.Card{&xfer.FileCard{UUID: name, Content: data}}, nil, nil
}

// hzLateDelta sends the seed as a delta whose base, an ordinary control
// artifact, follows it in the same request.
func hzLateDelta(x *hzStruct, name string, data []byte) ([]xfer.Card, []string, error) {
	b := &deck.Deck{Type: deck.Control, D: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), U: deck.User("u"),
		T: []deck.TagCard{{Type: deck.TagSingleton, Name: "late", UUID: x.c3}}}
	base, err := b.Marshal()
	if err != nil {
		return nil, nil, err
	}
	baseName := hash.SHA1(base)
	d, err := delta.Create(base, data)
	if err != nil {
		return nil, nil, err
	}
	return []xfer.Card{&xfer.FileCard{UUID: name, DeltaSrc: baseName, Content: d},
		&xfer.FileCard{UUID: baseName, Content: base}}, []string{baseName}, nil
}

// hzForeignStored: a control artifact under another implementation's seed
// label with no horizon-seed T card is ordinary, and is stored.
func hzForeignStored(t *testing.T) error {
	x := newHZStruct(t)
	name, data, err := hzForeignSeed(false)(x)
	if err != nil {
		return err
	}
	var pc string
	if err := x.s.f.r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc); err != nil {
		return err
	}
	req := &xfer.Message{Cards: []xfer.Card{&xfer.PushCard{ServerCode: "x", ProjectCode: pc},
		&xfer.FileCard{UUID: name, Content: data}}}
	resp, err := sync.HandleSync(context.Background(), x.s.f.r, req)
	if err != nil {
		return err
	}
	for _, c := range resp.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			return expectf("pushing a U zit-seed artifact: error %q", e.Message)
		}
	}
	if _, ok := blob.Exists(x.s.f.r.DB(), name); !ok {
		return expectf("the origin did not store a U zit-seed artifact with no horizon-seed card")
	}
	return nil
}

// hzSeedRefuse pushes the seed seedOf makes as cards builds it, after an
// ordinary artifact, and checks the refusal and that nothing was stored:
// not the seed, the ordinary artifact, nor the extra names.
func hzSeedRefuse(t *testing.T, seedOf func(x *hzStruct) (string, []byte, error),
	cards func(x *hzStruct, name string, data []byte) ([]xfer.Card, []string, error)) error {
	x := newHZStruct(t)
	name, data, err := seedOf(x)
	if err != nil {
		return err
	}
	seedCards, extra, err := cards(x, name, data)
	if err != nil {
		return err
	}
	var pc string
	if err := x.s.f.r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc); err != nil {
		return err
	}
	ordinary := []byte("an ordinary artifact pushed beside the seed\n")
	req := &xfer.Message{Cards: append([]xfer.Card{&xfer.PushCard{ServerCode: "x", ProjectCode: pc},
		&xfer.FileCard{UUID: hash.SHA1(ordinary), Content: ordinary}}, seedCards...)}
	resp, err := sync.HandleSync(context.Background(), x.s.f.r, req)
	if err != nil {
		return err
	}
	if len(resp.Cards) != 1 {
		return expectf("the reply has %d cards, want the refusal alone", len(resp.Cards))
	}
	if e, ok := resp.Cards[0].(*xfer.ErrorCard); !ok || e.Message != hzSeedRefused {
		return expectf("the reply is %#v, want error %q", resp.Cards[0], hzSeedRefused)
	}
	for _, u := range append([]string{name, hash.SHA1(ordinary)}, extra...) {
		if _, ok := blob.Exists(x.s.f.r.DB(), u); ok {
			return expectf("the refused request stored %.10s", u)
		}
	}
	return nil
}

var horizonPushCheckLate = Case{ID: "horizon.5.7.push-check-late-delta", Req: "HZ-5.7-2"}

// hzPushCheckLate: the §5.7 check is judged on expanded content as the seed
// check is. A stale cancellation sent as a delta whose base arrives later in
// the same push is refused whole, the base not stored either.
func hzPushCheckLate(t *testing.T) error {
	x := newHZStruct(t)
	x.s.tag(t, x.s.c4, "branch", tag.TagPropagating, "moved")
	x.s.tag(t, x.s.c4, "sym-moved", tag.TagPropagating, "")
	x.s.tag(t, x.s.c4, "sym-trunk", tag.TagCancel, "")
	nb := &deck.Deck{Type: deck.Checkin, C: "nb", D: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), U: deck.User("u"),
		P: []string{x.c4}, T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "nb"},
			{Type: deck.TagPropagating, Name: "sym-nb", UUID: "*"},
			{Type: deck.TagCancel, Name: "sym-trunk", UUID: "*"},
		}}
	data, err := nb.Marshal()
	if err != nil {
		return err
	}
	base := []byte("the base of a late delta\n")
	d, err := delta.Create(base, data)
	if err != nil {
		return err
	}
	var pc string
	if err := x.s.f.r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc); err != nil {
		return err
	}
	name, baseName := hash.SHA1(data), hash.SHA1(base)
	req := &xfer.Message{Cards: []xfer.Card{&xfer.PushCard{ServerCode: "x", ProjectCode: pc},
		&xfer.FileCard{UUID: name, DeltaSrc: baseName, Content: d}, &xfer.FileCard{UUID: baseName, Content: base}}}
	resp, err := sync.HandleSync(context.Background(), x.s.f.r, req)
	if err != nil {
		return err
	}
	if len(resp.Cards) != 1 {
		return expectf("the reply has %d cards, want the refusal alone", len(resp.Cards))
	}
	if e, ok := resp.Cards[0].(*xfer.ErrorCard); !ok || !strings.HasPrefix(e.Message, "stale seed: ") ||
		!strings.Contains(e.Message, "'moved'") {
		return expectf("the reply is %#v, want the stale-seed refusal naming moved", resp.Cards[0])
	}
	for _, u := range []string{name, baseName} {
		if _, ok := blob.Exists(x.s.f.r.DB(), u); ok {
			return expectf("the refused push stored %.10s", u)
		}
	}
	return nil
}

// hzSeedLaterRequest (B.4.4): a seed pushed as a delta whose base the
// origin lacks is stored dangling, since it cannot be judged. A later push
// of the base alone succeeds and stores the base; the seed, judged when it
// first becomes readable, is neither crosslinked nor served, and its tags
// never reach derived state, a rebuild's included. It is refused by its
// hash (F.1), kept stored but never linked or served. The refusal is
// store-local: a shun entry would reach every peer that pulls the shun
// group, and a horizon clone holding this, its current seed, would then
// drop it.
func hzSeedLaterRequest(t *testing.T) error {
	x := newHZStruct(t)
	r := x.s.f.r
	name, data, err := hzOwnSeed(x)
	if err != nil {
		return err
	}
	seedCards, extra, err := hzLateDelta(x, name, data)
	if err != nil {
		return err
	}
	if err := hzPushed(r, "the dangling delta", seedCards[0]); err != nil {
		return err
	}
	if err := hzPushed(r, "the base's push", seedCards[1]); err != nil {
		return err
	}
	for _, u := range extra {
		if !hzHeldReal(r, u) {
			return expectf("the base %.10s was not stored", u)
		}
	}
	return hzSeedInert(r, name)
}
