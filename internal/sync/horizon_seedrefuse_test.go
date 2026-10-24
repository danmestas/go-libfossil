package sync

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	"github.com/danmestas/go-libfossil/internal/shun"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/verify"
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

// A seed admitted as a dangling delta is judged when a later request makes
// it readable (spec F): that request is innocent and stored whole with no
// error, while the seed is recorded as refused, never crosslinked, announced
// or sent. The label and the shape are both caught.
func TestHorizonPushSeedBaseInLaterRequest(t *testing.T) {
	for _, shape := range []string{"label", "shape"} {
		o := newPCOrigin(t)
		baseCard, base := o.labelled("u", "b")
		var seed []byte
		if shape == "label" {
			_, seed = o.labelled(seedLogin, "a")
		} else {
			_, seed = o.bound("zit-seed")
		}
		s := deltaCard(t, base, baseCard.UUID, seed)
		if errs := errorsOf(o.push(s)); len(errs) != 0 {
			t.Fatalf("%s: the dangling delta: errors %q", shape, errs)
		}
		if errs := errorsOf(o.push(o.blobCard(), baseCard)); len(errs) != 0 {
			t.Fatalf("%s: the base's push: errors %q", shape, errs)
		}
		for _, u := range []string{baseCard.UUID, o.fileUID} {
			if phantom, err := isPhantomRow(o.r.DB(), u); err != nil || phantom || !o.held(u) {
				t.Fatalf("%s: %.10s not stored (%v)", shape, u, err)
			}
		}
		assertRefused(t, o.r, s.UUID, true)
		var linked int
		if err := o.r.DB().QueryRow(`SELECT (SELECT count(*) FROM tagxref JOIN blob ON blob.rid=tagxref.srcid
			WHERE blob.uuid=?1) + (SELECT count(*) FROM event JOIN blob ON blob.rid=event.objid WHERE blob.uuid=?1)`,
			s.UUID).Scan(&linked); err != nil {
			t.Fatal(err)
		}
		if linked != 0 {
			t.Fatalf("%s: the seed was crosslinked: %d rows", shape, linked)
		}
		if _, err := verify.Rebuild(o.r); err != nil {
			t.Fatal(err)
		}
		if err := o.r.DB().QueryRow(`SELECT count(*) FROM tagxref JOIN blob ON blob.rid=tagxref.srcid
			WHERE blob.uuid=?`, s.UUID).Scan(&linked); err != nil || linked != 0 {
			t.Fatalf("%s: rebuild crosslinked the seed: %d rows (%v)", shape, linked, err)
		}
		pull := o.push(&xfer.PullCard{ServerCode: "s", ProjectCode: testPC}, &xfer.GimmeCard{UUID: s.UUID})
		for _, c := range pull.Cards {
			if f, ok := c.(*xfer.FileCard); ok && f.UUID == s.UUID {
				t.Fatalf("%s: the seed was served", shape)
			}
			if g, ok := c.(*xfer.IGotCard); ok && g.UUID == s.UUID {
				t.Fatalf("%s: the seed was announced", shape)
			}
		}
	}
}

// Every dependent the base grounds is judged: an ordinary one before the
// seed is linked, and does not end the check. A forged dependent, whose
// bytes are a seed but not the artifact its name names, is not refused, so
// a forgery cannot bar the real artifact.
func TestHorizonPushSeedLaterJudgesEveryDependent(t *testing.T) {
	o := newPCOrigin(t)
	baseCard, base := o.labelled("u", "b")
	ordinaryCard, ordinary := o.labelled("u", "ordinary")
	_, seed := o.labelled(seedLogin, "a")
	_, forgedSeed := o.bound("zit-seed")
	plain := deltaCard(t, base, baseCard.UUID, ordinary)
	s := deltaCard(t, base, baseCard.UUID, seed)
	forged := deltaCard(t, base, baseCard.UUID, forgedSeed)
	forged.UUID = hash.SHA1([]byte("a name the forged bytes do not hash to\n"))
	// A plain file after the forgery: no link step judges it, so it is
	// cleared only if the forgery does not end the fill's judging.
	fileBytes := append(slices.Clone(base), []byte("a plain file on the base\n")...)
	file := deltaCard(t, base, baseCard.UUID, fileBytes)
	if errs := errorsOf(o.push(plain, forged, file, s)); len(errs) != 0 {
		t.Fatalf("the dangling deltas: errors %q", errs)
	}
	if errs := errorsOf(o.push(baseCard)); len(errs) != 0 {
		t.Fatalf("the base's push: errors %q", errs)
	}
	for _, c := range []struct {
		uuid string
		want bool
	}{{ordinaryCard.UUID, false}, {s.UUID, true}, {forged.UUID, false}} {
		assertRefused(t, o.r, c.uuid, c.want)
	}
	frid, _ := blob.Exists(o.r.DB(), file.UUID)
	if withheld, err := seedcheck.IsWithheld(o.r.DB(), frid); err != nil || withheld {
		t.Fatalf("the file after the forgery is withheld (%v)", err)
	}
	var linked int
	if err := o.r.DB().QueryRow(`SELECT count(*) FROM event JOIN blob ON blob.rid=event.objid
		WHERE blob.uuid=?`, ordinaryCard.UUID).Scan(&linked); err != nil || linked == 0 {
		t.Fatalf("the ordinary dependent was not linked: %d rows (%v)", linked, err)
	}
}

// A delta that cannot be read does not end the check: a seed after it in
// the same request is still refused.
func TestHorizonPushSeedAfterUnexpandable(t *testing.T) {
	o := newPCOrigin(t)
	_, never := o.labelled("u", "never")
	_, other := o.labelled("u", "other")
	stuck := deltaCard(t, never, hash.SHA1(never), other)
	baseCard, base := o.labelled("u", "b")
	_, seed := o.labelled(seedLogin, "a")
	s := deltaCard(t, base, baseCard.UUID, seed)
	resp := o.push(stuck, s, o.blobCard(), baseCard)
	assertSeedRefused(t, o, resp, stuck.UUID, s.UUID, o.fileUID, baseCard.UUID)
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

// isPhantomRow reports whether uuid names a row whose content the store
// does not hold.
func isPhantomRow(q db.Querier, uuid string) (bool, error) {
	var phantom bool
	err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM blob WHERE uuid=? AND size<0)", uuid).Scan(&phantom)
	return phantom, err
}

// assertRefused checks whether r records uuid as a refused seed: refused by
// its hash, kept stored, withheld from every reply and never linked. A
// refusal never lands in the shun table, which peers pull.
func assertRefused(t *testing.T, r *repo.Repo, uuid string, want bool) {
	t.Helper()
	refused, err := seedcheck.IsRefusedUUID(r.DB(), uuid)
	if err != nil || refused != want {
		t.Fatalf("%.10s refused = %v (%v), want %v", uuid, refused, err, want)
	}
	if shunned, err := shun.IsShunned(r.DB(), uuid); err != nil || shunned {
		t.Fatalf("%.10s is shunned (%v)", uuid, err)
	}
	if !want {
		return
	}
	rid, held := blob.Exists(r.DB(), uuid)
	if !held {
		t.Fatalf("the refused %.10s is not kept", uuid)
	}
	if withheld, err := seedcheck.IsWithheld(r.DB(), rid); err != nil || !withheld {
		t.Fatalf("the refused %.10s is not withheld (%v)", uuid, err)
	}
	if n := seedLinkRows(t, r, uuid); n != 0 {
		t.Fatalf("the refused %.10s was crosslinked: %d rows", uuid, n)
	}
}

// assertDropped checks that r refused uuid by its hash and never stored its
// content: a seed dropped on receipt (F.2).
func assertDropped(t *testing.T, r *repo.Repo, uuid string) {
	t.Helper()
	if refused, err := seedcheck.IsRefusedUUID(r.DB(), uuid); err != nil || !refused {
		t.Fatalf("%.10s is not refused (%v)", uuid, err)
	}
	if phantom, err := isPhantomRow(r.DB(), uuid); err != nil {
		t.Fatal(err)
	} else if _, held := blob.Exists(r.DB(), uuid); held && !phantom {
		t.Fatalf("the dropped %.10s was stored", uuid)
	}
}

// seedLinkRows counts the derived rows sourced from uuid: tag rows and its
// event.
func seedLinkRows(t *testing.T, r *repo.Repo, uuid string) int {
	t.Helper()
	var n int
	if err := r.DB().QueryRow(`SELECT (SELECT count(*) FROM tagxref JOIN blob ON blob.rid=tagxref.srcid
		WHERE blob.uuid=?1) + (SELECT count(*) FROM event JOIN blob ON blob.rid=event.objid WHERE blob.uuid=?1)`,
		uuid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// danglingSeed pushes a seed to o as a delta on base, which o lacks, and
// returns the seed's card. The base is named as a new repository names
// what it stores itself, by SHA3.
func danglingSeed(t *testing.T, o *pcOrigin, base []byte) *xfer.FileCard {
	t.Helper()
	_, seed := o.labelled(seedLogin, "a")
	s := deltaCard(t, base, hash.SHA3(base), seed)
	if errs := errorsOf(o.push(s)); len(errs) != 0 {
		t.Fatalf("the dangling delta: errors %q", errs)
	}
	return s
}

// The origin pulling the base from an upstream fills it through the client
// path: the seed is judged there too, refused and never linked.
func TestHorizonPullFillsSeedBase(t *testing.T) {
	o := newPCOrigin(t)
	upstream := setupPushableTestRepo(t)
	_, base := o.labelled("u", "b")
	s := danglingSeed(t, o, base)
	storeAndLink(t, upstream, base)
	res, err := Sync(context.Background(), o.r, handlerTransport(t, upstream),
		SyncOpts{Pull: true, ServerCode: "s", ProjectCode: testPC})
	if err != nil || len(res.Errors) != 0 {
		t.Fatalf("pull: %v %q", err, res.Errors)
	}
	if phantom, err := isPhantomRow(o.r.DB(), hash.SHA3(base)); err != nil || phantom {
		t.Fatalf("the base was not pulled (%v)", err)
	}
	assertRefused(t, o.r, s.UUID, true)
	if n := seedLinkRows(t, o.r, s.UUID); n != 0 {
		t.Fatalf("the seed was crosslinked: %d rows", n)
	}
}

// A local check-in that writes the base's bytes fills it too, and judges
// the seed in its own transaction: with no sweep after it, the seed is
// already refused, and neither a clone nor a pulling peer receives it.
func TestHorizonCheckinFillsSeedBase(t *testing.T) {
	o := newPCOrigin(t)
	base := []byte("file content that is also the seed's base\n")
	s := danglingSeed(t, o, base)
	if _, _, err := manifest.Checkin(o.r, manifest.CheckinOpts{Files: []manifest.File{{Name: "f", Content: base}},
		Comment: "f", User: "u", Parent: 0, Time: o.when.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, o.r, s.UUID, true)
	assertNotServed(t, o, s.UUID)
}

// assertNotServed checks that neither a clone of o nor a peer pulling from it
// receives uuid.
func assertNotServed(t *testing.T, o *pcOrigin, uuid string) {
	t.Helper()
	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "c.fossil"),
		handlerTransport(t, o.r), CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	if _, ok := blob.Exists(clone.DB(), uuid); ok {
		t.Fatalf("a clone received %.10s", uuid)
	}
	peer := setupSyncTestRepo(t)
	if _, err := Sync(context.Background(), peer, handlerTransport(t, o.r),
		SyncOpts{Pull: true, ServerCode: "s", ProjectCode: testPC}); err != nil {
		t.Fatal(err)
	}
	if _, ok := blob.Exists(peer.DB(), uuid); ok {
		t.Fatalf("a pulling peer received %.10s", uuid)
	}
}

// A seed still pending, its base not yet arrived, is withheld too: it has
// not been judged. (A clone of a store holding a dangling delta stalls
// whatever the delta is, so only a pull and a gimme are checked.)
func TestHorizonPendingSeedNotServed(t *testing.T) {
	o := newPCOrigin(t)
	_, base := o.labelled("u", "never sent")
	s := danglingSeed(t, o, base)
	resp, err := HandleSync(context.Background(), o.r, &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "s", ProjectCode: testPC}, &xfer.GimmeCard{UUID: s.UUID}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cards {
		switch c := c.(type) {
		case *xfer.FileCard:
			if c.UUID == s.UUID {
				t.Fatal("a gimme was answered with the pending seed")
			}
		case *xfer.IGotCard:
			if c.UUID == s.UUID {
				t.Fatal("an inventory announced the pending seed")
			}
		}
	}
}

// A pending row is cleared once judged, whatever the artifact: a plain file
// pushed as a delta ahead of its basis is served once the basis arrives.
func TestHorizonPendingFileClearedWhenJudged(t *testing.T) {
	o := newPCOrigin(t)
	base := []byte("the basis of a plain file\n")
	file := []byte("the basis of a plain file\nand a line more\n")
	d := deltaCard(t, base, hash.SHA3(base), file)
	if errs := errorsOf(o.push(d)); len(errs) != 0 {
		t.Fatalf("the dangling file: errors %q", errs)
	}
	rid, _ := blob.Exists(o.r.DB(), d.UUID)
	if withheld, err := seedcheck.IsWithheld(o.r.DB(), rid); err != nil || !withheld {
		t.Fatalf("the dangling file is not pending (%v)", err)
	}
	if errs := errorsOf(o.push(&xfer.FileCard{UUID: hash.SHA3(base), Content: base})); len(errs) != 0 {
		t.Fatalf("the base's push: errors %q", errs)
	}
	if withheld, err := seedcheck.IsWithheld(o.r.DB(), rid); err != nil || withheld {
		t.Fatalf("the judged file is still withheld (%v)", err)
	}
	peer := setupSyncTestRepo(t)
	if _, err := Sync(context.Background(), peer, handlerTransport(t, o.r),
		SyncOpts{Pull: true, ServerCode: "s", ProjectCode: testPC}); err != nil {
		t.Fatal(err)
	}
	if _, ok := blob.Exists(peer.DB(), d.UUID); !ok {
		t.Fatal("a pulling peer did not receive the file")
	}
}

// Purging a pending delta forgets its record, so an innocent artifact that
// is later stored under the reused rid is served.
func TestHorizonPurgedPendingForgotten(t *testing.T) {
	o := newPCOrigin(t)
	_, base := o.labelled("u", "never sent")
	s := danglingSeed(t, o, base)
	pendingRID, _ := blob.Exists(o.r.DB(), s.UUID)
	var maxRID libfossil.FslID
	if err := o.r.DB().QueryRow("SELECT max(rid) FROM blob").Scan(&maxRID); err != nil {
		t.Fatal(err)
	}
	if pendingRID != maxRID {
		t.Fatalf("fixture: the pending delta is rid %d, not the last row %d", pendingRID, maxRID)
	}
	if err := shun.Add(o.r.DB(), s.UUID, "purge"); err != nil {
		t.Fatal(err)
	}
	if _, err := shun.Purge(o.r.DB()); err != nil {
		t.Fatal(err)
	}
	innocent := []byte("an innocent blob stored after the purge\n")
	rid, _, err := blob.Store(o.r.DB(), innocent)
	if err != nil {
		t.Fatal(err)
	}
	if rid != pendingRID {
		t.Fatalf("fixture: the innocent blob is rid %d, not the reused %d", rid, pendingRID)
	}
	if withheld, err := seedcheck.IsWithheld(o.r.DB(), rid); err != nil || withheld {
		t.Fatalf("the innocent blob inherited the pending record (%v)", err)
	}
}

// A private sync's inventory withholds a seed pushed privately.
func TestHorizonSeedWithheldFromPrivateInventory(t *testing.T) {
	o := newPCOrigin(t)
	if _, err := o.r.DB().Exec("UPDATE user SET cap=cap||'x' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	_, base := o.labelled("u", "never sent")
	_, seed := o.labelled(seedLogin, "a")
	s := deltaCard(t, base, hash.SHA3(base), seed)
	private := &xfer.PragmaCard{Name: "send-private"}
	push := func(cards ...xfer.Card) *xfer.Message {
		req := &xfer.Message{Cards: append([]xfer.Card{private,
			&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}}, cards...)}
		resp, err := HandleSync(context.Background(), o.r, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if errs := errorsOf(push(&xfer.PrivateCard{}, s)); len(errs) != 0 {
		t.Fatalf("the private dangling seed: errors %q", errs)
	}
	resp, err := HandleSync(context.Background(), o.r, &xfer.Message{Cards: []xfer.Card{private,
		&xfer.PullCard{ServerCode: "s", ProjectCode: testPC}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cards {
		if g, ok := c.(*xfer.IGotCard); ok && g.UUID == s.UUID {
			t.Fatal("a private inventory announced the seed")
		}
	}
}

// A delta built on a pending seed is an ordinary artifact: once the base
// arrives the seed is refused but kept, so the delta on it is readable,
// linked and served. Its chain runs through the seed, whether it arrived
// after the seed (root the base) or before it (root the seed, moved to the
// base when the seed arrives).
func TestHorizonDeltaOnSeedServed(t *testing.T) {
	for _, order := range []string{"seed first", "dependent first"} {
		o := newPCOrigin(t)
		_, base := o.labelled("u", "b")
		_, seed := o.labelled(seedLogin, "a")
		s := deltaCard(t, base, hash.SHA3(base), seed)
		depCard, dep := o.labelled("u", "built on the seed")
		y := deltaCard(t, seed, s.UUID, dep)
		// A plain file on the seed too: no link step judges it, so only the
		// fill that grounds it clears it.
		file := append(slices.Clone(seed), []byte("a plain file built on the seed\n")...)
		f := deltaCard(t, seed, s.UUID, file)
		cards := []*xfer.FileCard{s, y, f}
		if order == "dependent first" {
			cards = []*xfer.FileCard{y, f, s}
		}
		for _, c := range cards {
			if errs := errorsOf(o.push(c)); len(errs) != 0 {
				t.Fatalf("%s: pushing %.10s: errors %q", order, c.UUID, errs)
			}
		}
		if errs := errorsOf(o.push(&xfer.FileCard{UUID: hash.SHA3(base), Content: base})); len(errs) != 0 {
			t.Fatalf("%s: the base's push: errors %q", order, errs)
		}
		assertRefused(t, o.r, s.UUID, true)
		rid, _ := blob.Exists(o.r.DB(), depCard.UUID)
		if !content.IsAvailable(o.r.DB(), rid) {
			t.Fatalf("%s: the delta on the seed is not readable", order)
		}
		if withheld, err := seedcheck.IsWithheld(o.r.DB(), rid); err != nil || withheld {
			t.Fatalf("%s: the delta on the seed is withheld (%v)", order, err)
		}
		if n := seedLinkRows(t, o.r, depCard.UUID); n == 0 {
			t.Fatalf("%s: the delta on the seed was not linked", order)
		}
		frid, _ := blob.Exists(o.r.DB(), f.UUID)
		if withheld, err := seedcheck.IsWithheld(o.r.DB(), frid); err != nil || withheld {
			t.Fatalf("%s: the file on the seed is withheld (%v)", order, err)
		}
		resp, err := HandleSync(context.Background(), o.r, &xfer.Message{Cards: []xfer.Card{
			&xfer.PullCard{ServerCode: "s", ProjectCode: testPC}, &xfer.GimmeCard{UUID: depCard.UUID}}})
		if err != nil {
			t.Fatal(err)
		}
		served := false
		for _, c := range resp.Cards {
			if f, ok := c.(*xfer.FileCard); ok && f.UUID == depCard.UUID {
				served = true
			}
		}
		if !served {
			t.Fatalf("%s: the delta on the seed was not served", order)
		}
	}
}

// A reply that read the withheld set for a gimme before the request stored
// a dangling seed still keeps the seed out of its inventory.
func TestHorizonSeedWithheldAfterEarlierGimme(t *testing.T) {
	o := newPCOrigin(t)
	_, base := o.labelled("u", "never sent")
	_, seed := o.labelled(seedLogin, "a")
	s := deltaCard(t, base, hash.SHA3(base), seed)
	resp, err := HandleSync(context.Background(), o.r, &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}, &xfer.PullCard{ServerCode: "s", ProjectCode: testPC},
		&xfer.GimmeCard{UUID: o.p}, s}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cards {
		if g, ok := c.(*xfer.IGotCard); ok && g.UUID == s.UUID {
			t.Fatal("the inventory announced the seed")
		}
	}
}

// A refused seed is served by no path: not cloned, and not pushed on by the
// origin acting as a client.
func TestHorizonRefusedSeedNotServed(t *testing.T) {
	o := newPCOrigin(t)
	_, base := o.labelled("u", "b")
	s := danglingSeed(t, o, base)
	baseCard := &xfer.FileCard{UUID: hash.SHA3(base), Content: base}
	if errs := errorsOf(o.push(baseCard)); len(errs) != 0 {
		t.Fatalf("the base's push: errors %q", errs)
	}
	assertRefused(t, o.r, s.UUID, true)
	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "c.fossil"),
		handlerTransport(t, o.r), CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	if _, ok := blob.Exists(clone.DB(), s.UUID); ok {
		t.Fatal("a clone received the refused seed")
	}
	if _, ok := blob.Exists(clone.DB(), baseCard.UUID); !ok {
		t.Fatal("a clone did not receive the base")
	}
	// The peer asks for the seed by name, as one that learned it elsewhere
	// would: the origin, as a client, still does not send it.
	peer := setupPushableTestRepo(t)
	asked := false
	tr := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		for _, c := range req.Cards {
			if f, ok := c.(*xfer.FileCard); ok && f.UUID == s.UUID {
				t.Error("the origin pushed the refused seed on")
			}
			if g, ok := c.(*xfer.IGotCard); ok && g.UUID == s.UUID {
				t.Error("the origin announced the refused seed")
			}
		}
		resp, err := HandleSync(context.Background(), peer, req)
		if err != nil {
			t.Errorf("HandleSync: %v", err)
			return &xfer.Message{}
		}
		if !asked {
			asked = true
			resp.Cards = append(resp.Cards, &xfer.GimmeCard{UUID: s.UUID})
		}
		return resp
	}}
	if _, err := Sync(context.Background(), o.r, tr,
		SyncOpts{Push: true, ServerCode: "s", ProjectCode: testPC}); err != nil {
		t.Fatal(err)
	}
}

// The refusal record is store-local: no configuration group carries it.
func TestSeedcheckTableNotSynced(t *testing.T) {
	for _, tbl := range configTables {
		if strings.Contains(tbl.table, "seedcheck") {
			t.Fatalf("configuration group %s carries the seedcheck table", tbl.card)
		}
	}
}

// An origin that refused a seed by its hash refuses a re-push of it at
// once, as full text or as a delta whose basis it lacks.
func TestHorizonRePushRefusedByHash(t *testing.T) {
	o := newPCOrigin(t)
	_, base := o.labelled("u", "b")
	s := danglingSeed(t, o, base)
	if errs := errorsOf(o.push(&xfer.FileCard{UUID: hash.SHA3(base), Content: base})); len(errs) != 0 {
		t.Fatalf("the base's push: errors %q", errs)
	}
	assertRefused(t, o.r, s.UUID, true)
	_, seed := o.labelled(seedLogin, "a")
	_, other := o.labelled("u", "elsewhere")
	for _, card := range []*xfer.FileCard{
		{UUID: s.UUID, Content: seed},
		deltaCard(t, other, hash.SHA3(other), seed),
	} {
		assertSeedRefused(t, o, o.push(o.blobCard(), card), o.fileUID)
		assertRefused(t, o.r, s.UUID, true)
	}
	// A pusher announcing the hash is not asked for it.
	for _, c := range o.push(&xfer.IGotCard{UUID: s.UUID}).Cards {
		if g, ok := c.(*xfer.GimmeCard); ok && g.UUID == s.UUID {
			t.Fatal("the origin asked for a refused seed a pusher announced")
		}
	}
}

// A tag whose control artifact is a pending seed's basis judges the seed in
// its own transaction: it is refused and dropped.
func TestHorizonTagFillsSeedBase(t *testing.T) {
	when := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	addTag := func(o *pcOrigin) libfossil.FslID {
		t.Helper()
		target, ok := blob.Exists(o.r.DB(), o.p)
		if !ok {
			t.Fatal("origin does not hold p")
		}
		rid, err := tag.AddTag(o.r, tag.TagOpts{TargetRID: target, TagName: "release", TagType: tag.TagSingleton,
			User: "u", Time: when})
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	// The same tag on an identical origin gives the control artifact's bytes.
	twin := newPCOrigin(t)
	base, err := content.Expand(twin.r.DB(), addTag(twin))
	if err != nil {
		t.Fatal(err)
	}
	o := newPCOrigin(t)
	s := danglingSeed(t, o, base)
	addTag(o)
	assertRefused(t, o.r, s.UUID, true)
}

// A pending seed made readable by a path that judges nothing (a raw store
// here) is still caught by the link step: it is refused and never linked.
func TestHorizonLinkJudgesPendingSeed(t *testing.T) {
	o := newPCOrigin(t)
	base := []byte("a basis stored with no seed check\n")
	s := danglingSeed(t, o, base)
	if _, _, err := blob.Store(o.r.DB(), base); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(o.r); err != nil {
		t.Fatal(err)
	}
	if refused, err := seedcheck.IsRefusedUUID(o.r.DB(), s.UUID); err != nil || !refused {
		t.Fatalf("the link step did not refuse the seed (%v)", err)
	}
	if n := seedLinkRows(t, o.r, s.UUID); n != 0 {
		t.Fatalf("the seed was crosslinked: %d rows", n)
	}
}

// seedPeer serves upstream to a client and records every request.
type seedPeer struct {
	t        *testing.T
	upstream *repo.Repo
	requests []*xfer.Message
}

func (p *seedPeer) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	p.requests = append(p.requests, req)
	return HandleSync(ctx, p.upstream, req)
}

// asked reports whether any request p saw gimmes uuid.
func (p *seedPeer) asked(uuid string) bool {
	for _, req := range p.requests {
		for _, c := range req.Cards {
			if g, ok := c.(*xfer.GimmeCard); ok && g.UUID == uuid {
				return true
			}
		}
	}
	return false
}

// Horizon F.2: a store never keeps a seed it did not receive through a
// horizon-seed announcement. A pull receiving one by label, or by binding
// card under another label, drops it: not stored, not counted, no phantom,
// and a second sync does not ask for it again.
func TestHorizonPullDropsSeed(t *testing.T) {
	for _, shape := range []string{"label", "card"} {
		src := newPCOrigin(t)
		var seed []byte
		if shape == "label" {
			_, seed = src.labelled(seedLogin, "a")
		} else {
			_, seed = src.bound("zit-seed")
		}
		// A peer that keeps seeds, as one predating F.2 does: stored raw,
		// since this store's own link step would refuse it.
		_, uuid, err := blob.Store(src.r.DB(), seed)
		if err != nil {
			t.Fatal(err)
		}
		client := setupSyncTestRepo(t)
		peer := &seedPeer{t: t, upstream: src.r}
		res, err := Sync(context.Background(), client, peer, SyncOpts{Pull: true, ServerCode: "s", ProjectCode: testPC})
		if err != nil || len(res.Errors) != 0 {
			t.Fatalf("%s: pull: %v %q", shape, err, res.Errors)
		}
		if !peer.asked(uuid) {
			t.Fatalf("%s: fixture: the client never asked for the seed", shape)
		}
		assertDropped(t, client, uuid)
		var held int
		if err := client.DB().QueryRow("SELECT count(*) FROM blob WHERE size>=0").Scan(&held); err != nil {
			t.Fatal(err)
		}
		if res.FilesRecvd != held {
			t.Fatalf("%s: %d files counted received, %d held", shape, res.FilesRecvd, held)
		}
		// A peer with no cursor for this client announces the seed again.
		other := setupPushableTestRepo(t)
		if _, _, err := blob.Store(other.DB(), seed); err != nil {
			t.Fatal(err)
		}
		peer = &seedPeer{t: t, upstream: other}
		if _, err := Sync(context.Background(), client, peer, SyncOpts{Pull: true, ServerCode: "s", ProjectCode: testPC}); err != nil {
			t.Fatal(err)
		}
		if peer.asked(uuid) {
			t.Fatalf("%s: a second sync asked for the dropped seed again", shape)
		}
	}
}

// A seed that arrives as a delta whose basis comes in a later round is
// dropped once the basis makes it readable.
func TestHorizonPullDropsLateSeedDelta(t *testing.T) {
	o := newPCOrigin(t)
	_, base := o.labelled("u", "b")
	_, seed := o.labelled(seedLogin, "a")
	baseUUID := hash.SHA3(base)
	s := deltaCard(t, base, baseUUID, seed)
	round := 0
	tr := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		round++
		if round == 1 {
			return &xfer.Message{Cards: []xfer.Card{s}}
		}
		for _, c := range req.Cards {
			if g, ok := c.(*xfer.GimmeCard); ok && g.UUID == baseUUID {
				return &xfer.Message{Cards: []xfer.Card{&xfer.FileCard{UUID: baseUUID, Content: base}}}
			}
		}
		return &xfer.Message{}
	}}
	client := setupSyncTestRepo(t)
	if _, err := Sync(context.Background(), client, tr, SyncOpts{Pull: true, ServerCode: "s", ProjectCode: testPC}); err != nil {
		t.Fatal(err)
	}
	if round < 2 {
		t.Fatal("fixture: the basis was never asked for")
	}
	assertRefused(t, client, s.UUID, true)
	if n := seedLinkRows(t, client, s.UUID); n != 0 {
		t.Fatalf("the seed was crosslinked: %d rows", n)
	}
	if phantom, err := isPhantomRow(client.DB(), baseUUID); err != nil || phantom {
		t.Fatalf("the basis was not stored (%v)", err)
	}
}

// A refused seed sent again unasked, as a delta whose basis the client
// lacks, is dropped by its hash: not stored and not counted.
func TestHorizonPullDropsResentSeed(t *testing.T) {
	o := newPCOrigin(t)
	_, seed := o.labelled(seedLogin, "a")
	_, base := o.labelled("u", "never sent")
	full := &xfer.FileCard{UUID: hash.SHA1(seed), Content: seed}
	resent := deltaCard(t, base, hash.SHA3(base), seed)
	client := setupSyncTestRepo(t)
	for i, card := range []*xfer.FileCard{full, resent} {
		sent := false
		tr := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
			if sent {
				return &xfer.Message{}
			}
			sent = true
			return &xfer.Message{Cards: []xfer.Card{card}}
		}}
		res, err := Sync(context.Background(), client, tr, SyncOpts{Pull: true, ServerCode: "s", ProjectCode: testPC})
		if err != nil {
			t.Fatal(err)
		}
		if res.FilesRecvd != 0 {
			t.Fatalf("send %d: %d files counted received", i, res.FilesRecvd)
		}
		assertDropped(t, client, full.UUID)
	}
}

// cloneScript answers a clone round by round from fixed replies; rounds past
// the last get the final one.
func cloneScript(replies ...[]xfer.Card) *MockTransport {
	round := 0
	return &MockTransport{Handler: func(*xfer.Message) *xfer.Message {
		cards := replies[min(round, len(replies)-1)]
		round++
		return &xfer.Message{Cards: cards}
	}}
}

// A clone drops a seed by label and by binding card, and a refused seed sent
// again in another form, counting none of them.
func TestHorizonCloneDropsSeed(t *testing.T) {
	o := newPCOrigin(t)
	_, labelled := o.labelled(seedLogin, "a")
	_, bound := o.bound("zit-seed")
	ordinary := []byte("an ordinary file the clone keeps\n")
	again := deltaCard(t, ordinary, hash.SHA1(ordinary), labelled)
	push := &xfer.PushCard{ServerCode: "s", ProjectCode: testPC}
	tr := cloneScript(
		[]xfer.Card{push,
			&xfer.FileCard{UUID: hash.SHA1(labelled), Content: labelled},
			&xfer.FileCard{UUID: hash.SHA1(bound), Content: bound},
			again,
			&xfer.FileCard{UUID: hash.SHA1(ordinary), Content: ordinary},
			&xfer.CloneSeqNoCard{SeqNo: 0}},
		[]xfer.Card{&xfer.CloneSeqNoCard{SeqNo: 0}},
	)
	clone, res, err := Clone(context.Background(), filepath.Join(t.TempDir(), "c.fossil"), tr, CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	if res.BlobsRecvd != 1 {
		t.Fatalf("%d blobs counted received, want the ordinary file alone", res.BlobsRecvd)
	}
	assertDropped(t, clone, hash.SHA1(labelled))
	assertDropped(t, clone, hash.SHA1(bound))
}

// A clone drops a seed sent as a delta whose basis comes in a later round.
func TestHorizonCloneDropsLateSeedDelta(t *testing.T) {
	o := newPCOrigin(t)
	_, base := o.labelled("u", "b")
	_, seed := o.labelled(seedLogin, "a")
	s := deltaCard(t, base, hash.SHA1(base), seed)
	push := &xfer.PushCard{ServerCode: "s", ProjectCode: testPC}
	tr := cloneScript(
		[]xfer.Card{push, s, &xfer.CloneSeqNoCard{SeqNo: 1}},
		[]xfer.Card{&xfer.FileCard{UUID: hash.SHA1(base), Content: base}, &xfer.CloneSeqNoCard{SeqNo: 0}},
		[]xfer.Card{&xfer.CloneSeqNoCard{SeqNo: 0}},
	)
	clone, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "c.fossil"), tr, CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	assertRefused(t, clone, s.UUID, true)
	if n := seedLinkRows(t, clone, s.UUID); n != 0 {
		t.Fatalf("the seed was crosslinked: %d rows", n)
	}
	if _, ok := blob.Exists(clone.DB(), hash.SHA1(base)); !ok {
		t.Fatal("the basis was not kept")
	}
}

// A seed sent to a client as a delta on a basis it holds is readable on
// arrival, and dropped then: never stored.
func TestHorizonPullDropsSeedDeltaOnHeldBase(t *testing.T) {
	o := newPCOrigin(t)
	_, base := o.labelled("u", "a basis the client holds")
	_, seed := o.labelled(seedLogin, "a")
	client := setupSyncTestRepo(t)
	if _, _, err := blob.Store(client.DB(), base); err != nil {
		t.Fatal(err)
	}
	s := deltaCard(t, base, hash.SHA3(base), seed)
	sent := false
	tr := &MockTransport{Handler: func(*xfer.Message) *xfer.Message {
		if sent {
			return &xfer.Message{}
		}
		sent = true
		return &xfer.Message{Cards: []xfer.Card{s}}
	}}
	res, err := Sync(context.Background(), client, tr, SyncOpts{Pull: true, ServerCode: "s", ProjectCode: testPC})
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesRecvd != 0 {
		t.Fatalf("%d files counted received", res.FilesRecvd)
	}
	assertDropped(t, client, s.UUID)
}
