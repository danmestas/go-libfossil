package sync

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
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// wantSeedRefused is the whole reply to a push carrying a seed (horizon
// §5.2.3 as amended by the zit 0.7.1 erratum): the error card alone.
const wantSeedRefused = "seed refused: a seed is private to the clone that holds it; it is never pushed"

// labelled is a control artifact tagging p under user, as a seed minted
// with that label is.
func (o *pcOrigin) labelled(user, value string) (*xfer.FileCard, []byte) {
	o.t.Helper()
	d := &deck.Deck{Type: deck.Control, D: o.when.Add(4 * time.Hour), U: deck.User(user),
		T: []deck.TagCard{{Type: deck.TagSingleton, Name: "seedtest", UUID: o.p, Value: value}}}
	data, err := d.Marshal()
	if err != nil {
		o.t.Fatal(err)
	}
	return &xfer.FileCard{UUID: hash.SHA1(data), Content: data}, data
}

// deltaCard is target sent as a delta against base, named baseUUID.
func deltaCard(t *testing.T, base []byte, baseUUID string, target []byte) *xfer.FileCard {
	t.Helper()
	d, err := delta.Create(base, target)
	if err != nil {
		t.Fatal(err)
	}
	return &xfer.FileCard{UUID: hash.SHA1(target), DeltaSrc: baseUUID, Content: d}
}

// assertSeedRefused checks that a reply is the refusal alone and that none
// of the named artifacts was stored.
func assertSeedRefused(t *testing.T, o *pcOrigin, resp *xfer.Message, uuids ...string) {
	t.Helper()
	if len(resp.Cards) != 1 {
		t.Fatalf("reply has %d cards %v, want the refusal alone", len(resp.Cards), resp.Cards)
	}
	if e, ok := resp.Cards[0].(*xfer.ErrorCard); !ok || e.Message != wantSeedRefused {
		t.Fatalf("reply = %#v, want error %q", resp.Cards[0], wantSeedRefused)
	}
	for _, u := range uuids {
		if o.held(u) {
			t.Fatalf("the refused request stored %s", u)
		}
	}
}

func TestHorizonPushSeedRefusedFullText(t *testing.T) {
	o := newPCOrigin(t)
	s, _ := o.labelled(seedLogin, "a")
	resp := o.push(o.blobCard(), s)
	assertSeedRefused(t, o, resp, s.UUID, o.fileUID)
}

func TestHorizonPushSeedRefusedDeltaHeldBase(t *testing.T) {
	o := newPCOrigin(t)
	rid, ok := blob.Exists(o.r.DB(), o.p)
	if !ok {
		t.Fatal("origin does not hold p")
	}
	base, err := content.Expand(o.r.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	_, seed := o.labelled(seedLogin, "a")
	s := deltaCard(t, base, o.p, seed)
	resp := o.push(o.blobCard(), s)
	assertSeedRefused(t, o, resp, s.UUID, o.fileUID)
}

func TestHorizonPushSeedRefusedLateBase(t *testing.T) {
	o := newPCOrigin(t)
	baseCard, base := o.labelled("u", "b")
	_, seed := o.labelled(seedLogin, "a")
	s := deltaCard(t, base, baseCard.UUID, seed)
	resp := o.push(s, o.blobCard(), baseCard)
	assertSeedRefused(t, o, resp, s.UUID, o.fileUID, baseCard.UUID)
}

// A delta whose base never arrives cannot be identified: it is stored as
// any dangling delta is, under the transit-phantom rules.
func TestHorizonPushSeedUnexpandableAdmitted(t *testing.T) {
	o := newPCOrigin(t)
	_, base := o.labelled("u", "b")
	_, seed := o.labelled(seedLogin, "a")
	s := deltaCard(t, base, hash.SHA1(base), seed)
	if errs := errorsOf(o.push(o.blobCard(), s)); len(errs) != 0 {
		t.Fatalf("errors = %q", errs)
	}
	if !o.held(o.fileUID) {
		t.Fatal("the ordinary artifact was not stored")
	}
}

// Only this origin's own label is refused: another implementation's seed
// label, and any user's, is an ordinary control artifact.
func TestHorizonPushForeignSeedLabelOrdinary(t *testing.T) {
	for _, user := range []string{"zit-seed", "zit-serve", "u", "Go-Libfossil"} {
		o := newPCOrigin(t)
		c, _ := o.labelled(user, "a")
		if errs := errorsOf(o.push(c)); len(errs) != 0 || !o.held(c.UUID) {
			t.Fatalf("U %s: errors %q, held %v", user, errs, o.held(c.UUID))
		}
	}
}

// A check-in carrying the label is not a seed: only control artifacts are.
func TestHorizonPushSeedLabelCheckinOrdinary(t *testing.T) {
	o := newPCOrigin(t)
	d := &deck.Deck{Type: deck.Checkin, C: "x", D: o.when.Add(2 * time.Hour), U: deck.User(seedLogin),
		P: []string{o.p}, F: []deck.FileCard{{Name: "c", UUID: o.fileUID}}}
	data, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	c := &xfer.FileCard{UUID: hash.SHA1(data), Content: data}
	if errs := errorsOf(o.push(o.blobCard(), c)); len(errs) != 0 || !o.held(c.UUID) {
		t.Fatalf("errors %q, held %v", errs, o.held(c.UUID))
	}
}

// §5.7 is judged on expanded content too: a stale cancellation sent as a
// delta whose base arrives later in the same push is refused.
func TestHorizonPushStaleCancelLateDelta(t *testing.T) {
	o := newPCOrigin(t)
	card, uuid := o.branchFrom("feat")
	d := deltaCard(t, o.file, o.fileUID, card.Content)
	errs := errorsOf(o.push(d, o.blobCard()))
	if len(errs) != 1 || !strings.HasPrefix(errs[0], "stale seed: ") {
		t.Fatalf("errors = %q, want one stale-seed refusal", errs)
	}
	if o.held(uuid) || o.held(o.fileUID) {
		t.Fatal("the refused push stored artifacts")
	}
}

// A pusher surfaces the refusal as its sync's error and stops.
func TestHorizonPushSeedRefusedClient(t *testing.T) {
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	origin := setupPushableTestRepo(t)
	client := setupSyncTestRepo(t)
	rid, _ := mbCommit(t, client, "root", 0, when)
	if _, err := tag.AddTag(client, tag.TagOpts{TargetRID: rid, TagName: "seedtest", TagType: tag.TagSingleton,
		Value: "a", User: seedLogin, Time: when.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	res, err := Sync(context.Background(), client, handlerTransport(t, origin),
		SyncOpts{Push: true, ServerCode: "s", ProjectCode: testPC})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Round one announces, round two sends what the origin asks for and is
	// refused; nothing is sent again.
	if res.Rounds != 2 || res.FilesSent != 3 {
		t.Fatalf("rounds %d, files sent %d: want the push to stop at the refusal", res.Rounds, res.FilesSent)
	}
	if len(res.Errors) != 1 || res.Errors[0] != "error: "+wantSeedRefused {
		t.Fatalf("errors = %q", res.Errors)
	}
	var n int
	if err := origin.DB().QueryRow("SELECT count(*) FROM blob WHERE size>=0").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("origin stored %d artifacts of a refused push", n)
	}
}

// bound is a control artifact under user carrying a horizon-seed T card, the
// shape of a bound seed whatever its label (horizon §5.2.3, spec E.2).
func (o *pcOrigin) bound(user string) (*xfer.FileCard, []byte) {
	o.t.Helper()
	d := &deck.Deck{Type: deck.Control, D: o.when.Add(4 * time.Hour), U: deck.User(user),
		T: []deck.TagCard{{Type: deck.TagSingleton, Name: "horizon-seed", UUID: o.p, Value: o.p}}}
	data, err := d.Marshal()
	if err != nil {
		o.t.Fatal(err)
	}
	return &xfer.FileCard{UUID: hash.SHA1(data), Content: data}, data
}

func TestHorizonPushSeedShapeRefusedForeignLabel(t *testing.T) {
	o := newPCOrigin(t)
	s, _ := o.bound("zit-seed")
	resp := o.push(o.blobCard(), s)
	assertSeedRefused(t, o, resp, s.UUID, o.fileUID)
}

func TestHorizonPushSeedShapeRefusedLateBase(t *testing.T) {
	o := newPCOrigin(t)
	baseCard, base := o.labelled("u", "b")
	_, seed := o.bound("zit-seed")
	s := deltaCard(t, base, baseCard.UUID, seed)
	resp := o.push(s, o.blobCard(), baseCard)
	assertSeedRefused(t, o, resp, s.UUID, o.fileUID, baseCard.UUID)
}
