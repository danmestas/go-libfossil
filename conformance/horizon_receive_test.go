package conformance

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// The receive rule (§5.2.3): a store never keeps a seed it did not receive
// through a horizon-seed announcement. On every receive path it drops an
// artifact that is a seed by its own label or by a binding card, stores one
// that carries neither as ordinary, does not count or ask again for what it
// dropped, keeps a record of the dropped hashes across sessions, and never
// announces or serves a delta it cannot yet expand.

func horizonReceiveCases() []Case {
	return []Case{horizonReceivePull, horizonReceiveClone, horizonReceiveLate, horizonReceiveLatePull,
		horizonReceiveLateOrigin, horizonReceiveRecord, horizonReceiveWithheld}
}

var (
	horizonReceivePull       = Case{ID: "horizon.5.2.3.receive-drop-pull", Req: "HZ-5.2-14"}
	horizonReceiveClone      = Case{ID: "horizon.5.2.3.receive-drop-clone", Req: "HZ-5.2-14"}
	horizonReceiveLate       = Case{ID: "horizon.5.2.3.receive-late-delta-unlinked", Req: "HZ-5.2-14"}
	horizonReceiveLatePull   = Case{ID: "horizon.5.2.3.receive-late-delta-pull-dropped", Req: "HZ-5.2-14"}
	horizonReceiveLateOrigin = Case{ID: "horizon.5.2.3.receive-late-delta-origin-dropped", Req: "HZ-5.2-14"}
	horizonReceiveRecord     = Case{ID: "horizon.5.2.3.receive-dropped-not-asked-again", Req: "HZ-5.2-15"}
	horizonReceiveWithheld   = Case{ID: "horizon.5.2.3.receive-dangling-withheld", Req: "HZ-5.2-16"}
)

const hzReceivePC = "0123456789abcdef0123456789abcdef01234567"

// hzReceiveRepo is a new store under the suite's project code.
func hzReceiveRepo(t *testing.T, name string) *repo.Repo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), name+".fossil"), "u", simio.CryptoRand{}, hzReceivePC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// hzControl is a control artifact by user tagging target, carrying a
// horizon-seed binding card when bound.
func hzControl(t *testing.T, user, tagName, target string, bound bool) []byte {
	t.Helper()
	cards := []deck.TagCard{{Type: deck.TagSingleton, Name: tagName, UUID: target}}
	if bound {
		cards = append(cards, deck.TagCard{Type: deck.TagSingleton, Name: seed.BindingTag, UUID: target,
			Value: "descendants"})
	}
	data, err := (&deck.Deck{Type: deck.Control, D: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		U: deck.User(user), T: cards}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// hzRecording serves upstream and records every request.
type hzRecording struct {
	upstream *repo.Repo
	requests []*xfer.Message
}

func (p *hzRecording) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	p.requests = append(p.requests, req)
	return sync.HandleSync(ctx, p.upstream, req)
}

func (p *hzRecording) asked(uuid string) bool {
	for _, req := range p.requests {
		for _, c := range req.Cards {
			if g, ok := c.(*xfer.GimmeCard); ok && g.UUID == uuid {
				return true
			}
		}
	}
	return false
}

// hzHeldReal reports whether r holds uuid's content.
func hzHeldReal(r *repo.Repo, uuid string) bool {
	var n int
	r.DB().QueryRow("SELECT count(*) FROM blob WHERE uuid=? AND size>=0", uuid).Scan(&n)
	return n > 0
}

// hzLinked reports whether uuid's tags reached r's derived state.
func hzLinked(r *repo.Repo, uuid string) bool {
	var n int
	r.DB().QueryRow("SELECT count(*) FROM tagxref JOIN blob ON blob.rid=tagxref.srcid WHERE blob.uuid=?", uuid).Scan(&n)
	return n > 0
}

func TestHorizonReceive(t *testing.T) {
	check(t, horizonReceivePull, func(t *testing.T) error { return hzReceivePull(t) })
	check(t, horizonReceiveClone, func(t *testing.T) error { return hzReceiveClone(t) })
	check(t, horizonReceiveLate, func(t *testing.T) error { return hzReceiveLate(t) })
	check(t, horizonReceiveLatePull, func(t *testing.T) error { return hzReceiveLatePull(t) })
	check(t, horizonReceiveLateOrigin, func(t *testing.T) error { return hzReceiveLateOrigin(t) })
	check(t, horizonReceiveRecord, func(t *testing.T) error { return hzReceiveRecord(t) })
	check(t, horizonReceiveWithheld, func(t *testing.T) error { return hzReceiveWithheld(t) })
}

// hzReceiveUpstream is a peer holding, stored raw as a peer predating the
// rule might, a seed by this store's label, a seed by a binding card under
// zit's label, and a control artifact under zit's label with neither.
func hzReceiveUpstream(t *testing.T) (*repo.Repo, []string, string) {
	t.Helper()
	up := hzReceiveRepo(t, "upstream")
	target := hash.SHA1([]byte("a target\n"))
	var seeds []string
	for _, data := range [][]byte{
		hzControl(t, seedcheck.Label, "alpha", target, false),
		hzControl(t, "zit-seed", "beta", target, true),
	} {
		_, uuid, err := blob.Store(up.DB(), data)
		if err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, uuid)
	}
	_, ordinary, err := blob.Store(up.DB(), hzControl(t, "zit-seed", "gamma", target, false))
	if err != nil {
		t.Fatal(err)
	}
	return up, seeds, ordinary
}

// hzReceivePull: a pull drops both seeds, counting neither, and stores the
// foreign artifact that carries neither label nor card as ordinary.
func hzReceivePull(t *testing.T) error {
	up, seeds, ordinary := hzReceiveUpstream(t)
	client := hzReceiveRepo(t, "client")
	peer := &hzRecording{upstream: up}
	res, err := sync.Sync(context.Background(), client, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	for _, s := range seeds {
		if !peer.asked(s) {
			return expectf("fixture: the client never asked for %.10s", s)
		}
		if hzHeldReal(client, s) {
			return expectf("the pulled seed %.10s was stored", s)
		}
	}
	if !hzHeldReal(client, ordinary) || !hzLinked(client, ordinary) {
		return expectf("the foreign artifact carrying neither label nor card is not an ordinary control artifact")
	}
	var held int
	client.DB().QueryRow("SELECT count(*) FROM blob WHERE size>=0").Scan(&held)
	if res.FilesRecvd != held {
		return expectf("%d files counted received, %d held", res.FilesRecvd, held)
	}
	return nil
}

// hzReceiveClone: a clone drops a seed it receives, counting it not.
func hzReceiveClone(t *testing.T) error {
	data := hzControl(t, seedcheck.Label, "alpha", hash.SHA1([]byte("a target\n")), false)
	ordinary := []byte("an ordinary file\n")
	tr := hzCloneScript(
		[]xfer.Card{&xfer.PushCard{ServerCode: "s", ProjectCode: hzReceivePC},
			&xfer.FileCard{UUID: hash.SHA1(data), Content: data},
			&xfer.FileCard{UUID: hash.SHA1(ordinary), Content: ordinary},
			&xfer.CloneSeqNoCard{SeqNo: 0}},
		[]xfer.Card{&xfer.CloneSeqNoCard{SeqNo: 0}},
	)
	clone, res, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "c.fossil"), tr, sync.CloneOpts{})
	if err != nil {
		return err
	}
	defer clone.Close()
	if hzHeldReal(clone, hash.SHA1(data)) {
		return expectf("the cloned seed was stored")
	}
	if res.BlobsRecvd != 1 {
		return expectf("%d blobs counted received, want the ordinary file alone", res.BlobsRecvd)
	}
	return nil
}

// hzLateSeed is this store's own seed sent as a delta on base, with a
// delta built on the seed: the cards that carry them, and the dependent's
// bytes.
type hzLateSeed struct {
	seed, dependent *xfer.FileCard
	dependentData   []byte
}

func newHZLateSeed(t *testing.T, base []byte) (hzLateSeed, error) {
	t.Helper()
	data := hzControl(t, seedcheck.Label, "alpha", hash.SHA1([]byte("a target\n")), false)
	d, err := delta.Create(base, data)
	if err != nil {
		return hzLateSeed{}, err
	}
	dep := append([]byte("an ordinary file built on the seed\n"), data...)
	dd, err := delta.Create(data, dep)
	if err != nil {
		return hzLateSeed{}, err
	}
	return hzLateSeed{
		seed:          &xfer.FileCard{UUID: hash.SHA1(data), DeltaSrc: hash.SHA1(base), Content: d},
		dependent:     &xfer.FileCard{UUID: hash.SHA1(dep), DeltaSrc: hash.SHA1(data), Content: dd},
		dependentData: dep,
	}, nil
}

// dropped checks r keeps no content for the seed, refuses it, and still
// reads the delta built on it.
func (l hzLateSeed) dropped(r *repo.Repo) error {
	if hzHeldReal(r, l.seed.UUID) {
		return expectf("the seed completed in a later round keeps a content row")
	}
	if hzLinked(r, l.seed.UUID) {
		return expectf("the seed completed in a later round was linked")
	}
	if refused, err := seedcheck.IsRefusedUUID(r.DB(), l.seed.UUID); err != nil || !refused {
		return expectf("the seed completed in a later round is not refused (%v)", err)
	}
	rid, ok := blob.Exists(r.DB(), l.dependent.UUID)
	if !ok {
		return expectf("the delta built on the seed was not stored")
	}
	got, err := content.Expand(r.DB(), rid)
	if err != nil || string(got) != string(l.dependentData) {
		return expectf("the delta built on the dropped seed no longer expands (%v)", err)
	}
	return nil
}

// hzHeldCount is how many artifacts r holds content for.
func hzHeldCount(r *repo.Repo) int {
	var held int
	r.DB().QueryRow("SELECT count(*) FROM blob WHERE size>=0").Scan(&held)
	return held
}

// hzReceiveLate: a seed a clone receives as a delta whose base comes in a
// later round is dropped once the base makes it readable, is not counted,
// and the delta built on it still expands.
func hzReceiveLate(t *testing.T) error {
	base := []byte("a base sent in the next round\n")
	l, err := newHZLateSeed(t, base)
	if err != nil {
		return err
	}
	tr := hzCloneScript(
		[]xfer.Card{&xfer.PushCard{ServerCode: "s", ProjectCode: hzReceivePC}, l.seed, l.dependent,
			&xfer.CloneSeqNoCard{SeqNo: 1}},
		[]xfer.Card{&xfer.FileCard{UUID: hash.SHA1(base), Content: base}, &xfer.CloneSeqNoCard{SeqNo: 0}},
		[]xfer.Card{&xfer.CloneSeqNoCard{SeqNo: 0}},
	)
	clone, res, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "c.fossil"), tr, sync.CloneOpts{})
	if err != nil {
		return err
	}
	defer clone.Close()
	if err := l.dropped(clone); err != nil {
		return err
	}
	if held := hzHeldCount(clone); res.BlobsRecvd != held {
		return expectf("%d blobs counted received, %d held", res.BlobsRecvd, held)
	}
	return nil
}

// hzReceiveLatePull: the same on a pull, whose base arrives in a later
// reply: the seed is dropped, not counted, and not asked for again.
func hzReceiveLatePull(t *testing.T) error {
	base := []byte("a base the next reply sends\n")
	l, err := newHZLateSeed(t, base)
	if err != nil {
		return err
	}
	replies := [][]xfer.Card{{l.seed, l.dependent}, {&xfer.FileCard{UUID: hash.SHA1(base), Content: base}}}
	var asked []*xfer.Message
	tr := &sync.MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		asked = append(asked, req)
		if len(asked) > len(replies) {
			return &xfer.Message{}
		}
		return &xfer.Message{Cards: replies[len(asked)-1]}
	}}
	client := hzReceiveRepo(t, "client")
	res, err := sync.Sync(context.Background(), client, tr, sync.SyncOpts{Pull: true, ProjectCode: hzReceivePC})
	if err != nil {
		return err
	}
	if err := l.dropped(client); err != nil {
		return err
	}
	if held := hzHeldCount(client); res.FilesRecvd != held {
		return expectf("%d files counted received, %d held", res.FilesRecvd, held)
	}
	for _, req := range asked[min(2, len(asked)):] {
		for _, c := range req.Cards {
			if g, ok := c.(*xfer.GimmeCard); ok && g.UUID == l.seed.UUID {
				return expectf("the dropped seed was asked for again")
			}
		}
	}
	return nil
}

// hzReceiveLateOrigin: an origin that stores a pushed seed as a delta whose
// base only a later request supplies drops it then, keeps the delta built on
// it readable, and does not ask the pusher for it.
func hzReceiveLateOrigin(t *testing.T) error {
	base := []byte("a base the next request pushes\n")
	l, err := newHZLateSeed(t, base)
	if err != nil {
		return err
	}
	origin := hzReceiveRepo(t, "origin")
	// Anyone may push, so the requests carry no login.
	if _, err := origin.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		return err
	}
	push := func(cards ...xfer.Card) (*xfer.Message, error) {
		req := &xfer.Message{Cards: append([]xfer.Card{&xfer.PushCard{ServerCode: "s", ProjectCode: hzReceivePC}},
			cards...)}
		return sync.HandleSync(context.Background(), origin, req)
	}
	r1, err := push(l.seed, l.dependent)
	if err != nil {
		return err
	}
	if errs := hzErrors(r1); len(errs) != 0 {
		return expectf("the dangling push: errors %q", errs)
	}
	resp, err := push(&xfer.FileCard{UUID: hash.SHA1(base), Content: base})
	if err != nil {
		return err
	}
	if errs := hzErrors(resp); len(errs) != 0 {
		return expectf("the base's push: errors %q", errs)
	}
	if err := l.dropped(origin); err != nil {
		return err
	}
	for _, c := range resp.Cards {
		if g, ok := c.(*xfer.GimmeCard); ok && g.UUID == l.seed.UUID {
			return expectf("the origin asked the pusher for the dropped seed")
		}
	}
	return nil
}

// hzReceiveRecord: a later session, from another peer that announces the
// dropped seeds, does not ask for them again.
func hzReceiveRecord(t *testing.T) error {
	up, seeds, _ := hzReceiveUpstream(t)
	client := hzReceiveRepo(t, "client")
	if _, err := sync.Sync(context.Background(), client, &hzRecording{upstream: up}, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	other, _, _ := hzReceiveUpstream(t)
	peer := &hzRecording{upstream: other}
	if _, err := sync.Sync(context.Background(), client, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	for _, s := range seeds {
		if peer.asked(s) {
			return expectf("a later session asked for the dropped %.10s again", s)
		}
	}
	return nil
}

// hzReceiveWithheld: a pulled delta whose base never arrives is kept, but a
// peer pulling from this store is neither announced nor sent it.
func hzReceiveWithheld(t *testing.T) error {
	base := []byte("a base the peer never sends\n")
	target := []byte("an ordinary file sent as a delta on it\n")
	d, err := delta.Create(base, target)
	if err != nil {
		return err
	}
	card := &xfer.FileCard{UUID: hash.SHA1(target), DeltaSrc: hash.SHA1(base), Content: d}
	sent := false
	tr := &sync.MockTransport{Handler: func(*xfer.Message) *xfer.Message {
		if sent {
			return &xfer.Message{}
		}
		sent = true
		return &xfer.Message{Cards: []xfer.Card{card}}
	}}
	client := hzReceiveRepo(t, "client")
	if _, err := sync.Sync(context.Background(), client, tr, sync.SyncOpts{Pull: true, ProjectCode: hzReceivePC}); err != nil {
		return err
	}
	if _, ok := blob.Exists(client.DB(), card.UUID); !ok {
		return expectf("fixture: the dangling delta was not kept")
	}
	resp, err := sync.HandleSync(context.Background(), client, &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "s", ProjectCode: hzReceivePC}, &xfer.GimmeCard{UUID: card.UUID}}})
	if err != nil {
		return err
	}
	for _, c := range resp.Cards {
		switch c := c.(type) {
		case *xfer.IGotCard:
			if c.UUID == card.UUID {
				return expectf("the unexpandable delta was announced")
			}
		case *xfer.FileCard:
			if c.UUID == card.UUID {
				return expectf("the unexpandable delta was sent")
			}
		case *xfer.CFileCard:
			if c.UUID == card.UUID {
				return expectf("the unexpandable delta was sent")
			}
		}
	}
	return nil
}

// hzCloneScript answers a clone round by round from fixed replies; rounds
// past the last get the final one.
func hzCloneScript(replies ...[]xfer.Card) *sync.MockTransport {
	round := 0
	return &sync.MockTransport{Handler: func(*xfer.Message) *xfer.Message {
		cards := replies[min(round, len(replies)-1)]
		round++
		return &xfer.Message{Cards: cards}
	}}
}
