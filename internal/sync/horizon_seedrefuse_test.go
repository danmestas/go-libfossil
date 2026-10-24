package sync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
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
	if errs := errorsOf(o.push(plain, forged, s)); len(errs) != 0 {
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

// assertRefused checks whether r records uuid as a refused seed, and that
// no seed refusal ever lands in the shun table, which peers pull.
func assertRefused(t *testing.T, r *repo.Repo, uuid string, want bool) {
	t.Helper()
	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatalf("%.10s is not held", uuid)
	}
	refused, err := seedcheck.IsRefused(r.DB(), rid)
	if err != nil || refused != want {
		t.Fatalf("%.10s refused = %v (%v), want %v", uuid, refused, err, want)
	}
	if shunned, err := shun.IsShunned(r.DB(), uuid); err != nil || shunned {
		t.Fatalf("%.10s is shunned (%v)", uuid, err)
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

// A local check-in that writes the base's bytes fills it too; the sweep
// after it judges the seed.
func TestHorizonCheckinFillsSeedBase(t *testing.T) {
	o := newPCOrigin(t)
	base := []byte("file content that is also the seed's base\n")
	s := danglingSeed(t, o, base)
	if _, _, err := manifest.Checkin(o.r, manifest.CheckinOpts{Files: []manifest.File{{Name: "f", Content: base}},
		Comment: "f", User: "u", Parent: 0, Time: o.when.Add(5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(o.r); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, o.r, s.UUID, true)
	if n := seedLinkRows(t, o.r, s.UUID); n != 0 {
		t.Fatalf("the seed was crosslinked: %d rows", n)
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
