//go:build !js && !windows

package conformance

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/clearsign/signtest"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The binding cases (horizon §5.2.1, §5.2.4) run over hzStruct. Every seed
// carries one binding card, T +horizon-seed <C> <rule>, inside its signed
// bytes; a clone holds a seed only when that card names its own horizon and
// the seed is dated no earlier than the one it holds, and refuses any other
// as a wrongly signed one, falling back to the content horizon. A refused
// seed reaches the clone through hzSwapSeed, which stands in for an origin
// or a connection that serves the wrong seed.

func horizonSeedBindCases() []Case {
	return []Case{horizonBindingCard, horizonBindOtherC, horizonBindOtherRule, horizonBindNone, horizonBindTwice,
		horizonBindReplay, horizonBindNamespace, horizonBindCommand, horizonBindNotDerived}
}

var (
	horizonBindingCard    = Case{ID: "horizon.5.2.1.seed-binding-card", Req: "HZ-5.2-8"}
	horizonBindOtherC     = Case{ID: "horizon.5.2.4.seed-other-c-refused", Req: "HZ-5.2-11"}
	horizonBindOtherRule  = Case{ID: "horizon.5.2.4.seed-other-rule-refused", Req: "HZ-5.2-11"}
	horizonBindNone       = Case{ID: "horizon.5.2.4.seed-unbound-refused", Req: "HZ-5.2-11"}
	horizonBindTwice      = Case{ID: "horizon.5.2.4.seed-two-bindings-refused", Req: "HZ-5.2-11"}
	horizonBindReplay     = Case{ID: "horizon.5.2.4.seed-older-refused", Req: "HZ-5.2-11"}
	horizonBindNamespace  = Case{ID: "horizon.5.2.4.seed-ssh-namespace", Req: "HZ-5.2-12"}
	horizonBindCommand    = Case{ID: "horizon.5.2.4.seed-command-apart", Req: "HZ-5.2-12"}
	horizonBindNotDerived = Case{ID: "horizon.5.2.1.binding-not-derived", Req: "HZ-5.2-13"}
)

func TestHorizonSeedBinding(t *testing.T) {
	check(t, horizonBindingCard, func() error { return hzBindingCard(t) })
	check(t, horizonBindOtherC, func() error {
		return hzBindRefused(t, func(x *hzStruct) (string, []byte, error) {
			k, err := horizon.HeldSet(context.Background(), x.s.f.r.DB(), x.s.c4, cone.RuleDescendants)
			if err != nil {
				return "", nil, err
			}
			return seed.Mint(x.s.f.r.DB(), x.s.c4, cone.RuleDescendants, k, seedLoginGo)
		})
	})
	check(t, horizonBindOtherRule, func() error {
		return hzBindRefused(t, func(x *hzStruct) (string, []byte, error) {
			return seed.Mint(x.s.f.r.DB(), x.s.c3, cone.RuleLeaves, x.s.k, seedLoginGo)
		})
	})
	check(t, horizonBindNone, func() error {
		return hzBindRefused(t, hzRebound(func(cards []deck.TagCard, _ *hzStruct) []deck.TagCard {
			return slices.DeleteFunc(cards, func(tc deck.TagCard) bool { return tc.Name == seed.BindingTag })
		}))
	})
	check(t, horizonBindTwice, func() error {
		// The extra card sorts before C's, so C's is the last a reader sees.
		return hzBindRefused(t, hzRebound(func(cards []deck.TagCard, _ *hzStruct) []deck.TagCard {
			return append(cards, deck.TagCard{Type: deck.TagSingleton, Name: seed.BindingTag,
				UUID: strings.Repeat("0", 40), Value: "descendants"})
		}))
	})
	check(t, horizonBindReplay, func() error { return hzBindReplay(t) })
	check(t, horizonBindNamespace, func() error { return hzBindNamespace(t) })
	check(t, horizonBindCommand, func() error { return hzBindCommand(t) })
	check(t, horizonBindNotDerived, func() error { return hzBindNotDerived(t) })
}

// hzSwapSeed makes a reply's seed name and data: it renames the reply's
// horizon-seed announcement and replaces the card it named, or, in a reply
// serving the structural level that carries none, announces and sends the
// seed as an origin re-seeding does.
func hzSwapSeed(name string, data []byte) func(*xfer.Message) {
	return func(m *xfer.Message) {
		old := ""
		for i, card := range m.Cards {
			if p, ok := card.(*xfer.PragmaCard); ok && p.Name == "horizon-seed" && len(p.Values) == 1 {
				old, p.Values[0] = p.Values[0], name
				continue
			}
			switch c := card.(type) {
			case *xfer.FileCard:
				if old != "" && c.UUID == old {
					m.Cards[i] = &xfer.FileCard{UUID: name, Content: data}
				}
			case *xfer.CFileCard:
				if old != "" && c.UUID == old {
					m.Cards[i] = &xfer.FileCard{UUID: name, Content: data}
				}
			}
		}
		if served := hzPragmas(m, "horizon-served"); old == "" && len(served) == 1 && served[0][0] == "structural" {
			m.Cards = append(m.Cards, &xfer.PragmaCard{Name: "horizon-seed", Values: []string{name}},
				&xfer.FileCard{UUID: name, Content: data})
		}
	}
}

// hzRebound is the origin's seed for C with its T cards edited and the
// artifact sealed again under its new name.
func hzRebound(edit func([]deck.TagCard, *hzStruct) []deck.TagCard) func(*hzStruct) (string, []byte, error) {
	return func(x *hzStruct) (string, []byte, error) {
		_, data, err := x.s.mint(seedLoginGo)
		if err != nil {
			return "", nil, err
		}
		d, err := deck.Parse(data)
		if err != nil {
			return "", nil, err
		}
		out, err := (&deck.Deck{Type: deck.Control, D: d.D, T: edit(slices.Clone(d.T), x), U: d.U}).Marshal()
		if err != nil {
			return "", nil, err
		}
		return hash.SHA3(out), out, nil
	}
}

// hzBindFellBack checks a structural store that refused its seed in a
// transfer that reported fell: the transfer completed, and the store holds
// no seed and is stored at the content horizon.
func hzBindFellBack(r *repo.Repo, fell bool, err error) error {
	if err != nil {
		return expectf("the transfer failed instead of falling back: %v", err)
	}
	if !fell {
		return expectf("the store did not fall back from a seed it must refuse")
	}
	if held, _ := seed.Held(r.DB()); held != nil {
		return expectf("the store holds a seed it must refuse:\n%s", held)
	}
	if d, err := hzStructDepth(r); err != nil || d != cone.DepthContent {
		return expectf("stored depth %v (%v), want content", d, err)
	}
	return nil
}

// hzBindRefused: a clone given a seed that is not bound to its horizon by
// exactly one binding card naming C and its rule refuses it, falls back to
// the content horizon and completes; a pull then keeps the content horizon.
func hzBindRefused(t *testing.T, wrong func(*hzStruct) (string, []byte, error)) error {
	x := newHZStruct(t)
	name, data, err := wrong(x)
	if err != nil {
		return err
	}
	r, result, err := x.clone(t, &hzRewritingPeer{inner: x.peer(), rewrite: hzSwapSeed(name, data)})
	if err != nil {
		return expectf("the clone failed instead of falling back: %v", err)
	}
	if err := hzBindFellBack(r, result.HorizonFallback, nil); err != nil {
		return err
	}
	pulled, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true})
	if err != nil {
		return err
	}
	if d, err := hzStructDepth(r); err != nil || d != cone.DepthContent || pulled.StructuralServed != 0 {
		return expectf("after a pull: depth %v (%v), structural in %d rounds; want the content horizon kept",
			d, err, pulled.StructuralServed)
	}
	return nil
}

// hzBindingCard: the seed an origin serves carries exactly one binding card,
// a singleton horizon-seed tag on C whose value is the rule, inside the
// signed bytes when it is signed; the clone holds it and derives from it no
// horizon-seed tag, so K derives as it does densely.
func hzBindingCard(t *testing.T) error {
	x := newHZStruct(t)
	command, pub := signtest.SSHKey(t, "ed25519", clearsign.SeedNamespace)
	hzSigningOrigin(t, x, command)
	r, result, err := x.keyedClone(t, x.peer(), pub)
	if err != nil {
		return err
	}
	if result.HorizonFallback {
		return expectf("a clone fell back from its origin's bound seed")
	}
	held := mustHeld(t, r)
	signedBody := deck.StripClearsign(held)
	if line := "T +horizon-seed " + x.c3 + " descendants\n"; bytes.Count(signedBody, []byte(line)) != 1 ||
		bytes.Count(held, []byte("horizon-seed")) != 1 {
		return expectf("the held seed does not carry %q exactly once, signed:\n%s", line, held)
	}
	if n := hzBindingRows(r); n != 0 {
		return expectf("the clone derived %d horizon-seed rows from its seed", n)
	}
	return x.s.derivesDense(t, r)
}

// hzBindingRows counts the horizon-seed tag rows r derives.
func hzBindingRows(r *repo.Repo) int {
	var n int
	r.DB().QueryRow(`SELECT count(*) FROM tagxref JOIN tag USING(tagid) WHERE tagname='horizon-seed'`).Scan(&n)
	return n
}

// hzBindReplay: a seed dated before the one held is a replay: a pull that
// delivers it refuses it and falls back. One dated the same as the held one
// is accepted. A deepen discards the held seed first, so the seed for C',
// dated earlier than the one for C, is held.
func hzBindReplay(t *testing.T) error {
	x := newHZStruct(t)
	oldName, oldData, err := x.s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	x.s.tag(t, x.s.c3, "sym-v2", tag.TagSingleton, "")
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	held := mustHeld(t, r)
	sameName, sameData, err := x.s.mint("zit-seed")
	if err != nil {
		return err
	}
	if !hzSameD(held, sameData) || sameName == hash.SHA3(held) || hzSameD(held, oldData) {
		return expectf("fixture: the held, equal and older seeds are not dated as the case needs")
	}
	same, err := sync.Sync(context.Background(), r, &hzRewritingPeer{inner: x.peer(), rewrite: hzSwapSeed(sameName, sameData)},
		sync.SyncOpts{Pull: true})
	if err != nil || same.HorizonFallback {
		return expectf("a seed dated as the held one: %v, fell back %v; want it accepted", err, same != nil && same.HorizonFallback)
	}
	if got := mustHeld(t, r); !bytes.Equal(got, sameData) {
		return expectf("the equally dated seed is not held:\n%s", got)
	}
	replayed, err := sync.Sync(context.Background(), r, &hzRewritingPeer{inner: x.peer(), rewrite: hzSwapSeed(oldName, oldData)},
		sync.SyncOpts{Pull: true})
	if err := hzBindFellBack(r, replayed != nil && replayed.HorizonFallback, err); err != nil {
		return expectf("a replayed older seed: %v", err)
	}
	return hzBindDeepenFloor(t)
}

// hzBindDeepenFloor: a clone at c3 holds a seed dated after a seed for c2;
// deepening to c2 discards it first, so the earlier seed for c2 is held. An
// honest origin dates every seed at the repository's latest tag event
// (§5.2.2), so the earlier seed for c2 is one minted before a later tag
// event, served by a peer that replays it.
func hzBindDeepenFloor(t *testing.T) error {
	x := newHZStruct(t)
	k2, err := horizon.HeldSet(context.Background(), x.s.f.r.DB(), x.s.c2, cone.RuleDescendants)
	if err != nil {
		return err
	}
	oldName, oldData, err := seed.Mint(x.s.f.r.DB(), x.s.c2, cone.RuleDescendants, k2, seedLoginGo)
	if err != nil {
		return err
	}
	x.s.tag(t, x.s.c3, "sym-v2", tag.TagSingleton, "")
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	atC := mustHeld(t, r)
	res, err := sync.Deepen(context.Background(), r, &hzRewritingPeer{inner: x.peer(), rewrite: hzSwapSeed(oldName, oldData)},
		x.c2, sync.SyncOpts{})
	if err != nil || res.HorizonFallback {
		return expectf("deepen to c2: %v, fell back %v", err, res != nil && res.HorizonFallback)
	}
	atC2 := mustHeld(t, r)
	a, err := deck.Parse(atC)
	if err != nil {
		return err
	}
	b, err := deck.Parse(atC2)
	if err != nil {
		return err
	}
	if !b.D.Before(a.D) {
		return expectf("fixture: the seed for c2 (%v) is not dated before the seed for c3 (%v)", b.D, a.D)
	}
	return hzStoredHorizon(r, x.c2, cone.DepthStructural)
}

// hzSameD reports whether two seeds carry the same D card.
func hzSameD(a, b []byte) bool {
	da, errA := deck.Parse(a)
	db, errB := deck.Parse(b)
	return errA == nil && errB == nil && da.D.Equal(db.D)
}

// hzBindNamespace: an SSH signature over the seed made under the check-in
// namespace, fossilscm, does not verify as a seed's, so a keyed clone that
// receives one falls back; the same seed signed under fossil-horizon-seed by
// the same key is held.
func hzBindNamespace(t *testing.T) error {
	x := newHZStruct(t)
	checkin, pub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	_, unsigned, err := x.s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	underCheckin, err := (&clearsign.Signer{Command: checkin}).Sign(context.Background(), unsigned)
	if err != nil {
		return err
	}
	seedCommand := strings.Replace(checkin, "-n "+clearsign.SSHNamespace, "-n "+clearsign.SeedNamespace, 1)
	underSeed, err := (&clearsign.Signer{Command: seedCommand}).Sign(context.Background(), unsigned)
	if err != nil {
		return err
	}
	r, result, err := x.keyedClone(t, &hzRewritingPeer{inner: x.peer(),
		rewrite: hzSwapSeed(hash.SHA3(underCheckin), underCheckin)}, pub)
	if err := hzBindFellBack(r, result != nil && result.HorizonFallback, err); err != nil {
		return expectf("a seed signed in namespace %s: %v", clearsign.SSHNamespace, err)
	}
	r, result, err = x.keyedClone(t, &hzRewritingPeer{inner: x.peer(),
		rewrite: hzSwapSeed(hash.SHA3(underSeed), underSeed)}, pub)
	if err != nil || result.HorizonFallback {
		return expectf("a seed signed in namespace %s: %v, fell back %v", clearsign.SeedNamespace, err,
			result != nil && result.HorizonFallback)
	}
	if !bytes.Equal(mustHeld(t, r), underSeed) {
		return expectf("the seed signed in %s is not held", clearsign.SeedNamespace)
	}
	return nil
}

// hzBindCommand: an origin's seed-signing command is configured apart from
// its check-in signing: one that signs check-ins, with clearsign and
// pgp-command set, and has no seed command sends its seed unsigned, which a
// keyed clone refuses; one whose seed command signs under the check-in
// namespace serves no seed signed so.
func hzBindCommand(t *testing.T) error {
	x := newHZStruct(t)
	checkin, pub := signtest.SSHKey(t, "ed25519", clearsign.SSHNamespace)
	for _, kv := range [][2]string{{clearsign.SettingClearsign, "on"}, {clearsign.SettingPGPCommand, checkin}} {
		if err := x.s.f.r.SetConfig(kv[0], kv[1]); err != nil {
			return err
		}
	}
	r, _, err := x.keyedClone(t, x.peer(), "")
	if err != nil {
		return err
	}
	if held := mustHeld(t, r); bytes.Contains(held, []byte("SIGNED MESSAGE")) {
		return expectf("an origin with no seed command signed its seed:\n%s", held)
	}
	r, result, err := x.keyedClone(t, x.peer(), pub)
	if err := hzBindFellBack(r, result != nil && result.HorizonFallback, err); err != nil {
		return expectf("a keyed clone of an origin signing only check-ins: %v", err)
	}
	hzSigningOrigin(t, x, checkin)
	if _, _, err := x.keyedClone(t, x.peer(), ""); err == nil || !strings.Contains(err.Error(), clearsign.SeedNamespace) {
		return expectf("an origin whose seed command signs in %s: %v, want it to send no seed signed so",
			clearsign.SSHNamespace, err)
	}
	return nil
}

// hzBindNotDerived: an origin holding a horizon-seed tag in force at C, as a
// control artifact stored before seeds were refused by shape could leave it,
// never derives it into a seed: the seed carries its own binding card alone,
// and with no other tag in force there is no seed.
func hzBindNotDerived(t *testing.T) error {
	x := newHZStruct(t)
	x.s.tag(t, x.s.c3, seed.BindingTag, tag.TagSingleton, "leaves")
	if n := hzBindingRows(x.s.f.r); n == 0 {
		return expectf("fixture: the origin holds no horizon-seed row")
	}
	_, data, err := x.s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	if got := bytes.Count(data, []byte("T +horizon-seed ")); got != 1 ||
		!bytes.Contains(data, []byte("T +horizon-seed "+x.c3+" descendants\n")) {
		return expectf("the seed carries %d horizon-seed cards, want its own binding card alone:\n%s", got, data)
	}
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	if !bytes.Equal(mustHeld(t, r), data) {
		return expectf("the clone holds another seed than the origin mints")
	}
	if _, err := x.s.f.r.DB().Exec(`DELETE FROM tagxref WHERE tagid NOT IN
		(SELECT tagid FROM tag WHERE tagname=?)`, seed.BindingTag); err != nil {
		return err
	}
	if _, _, err := x.s.mint(seedLoginGo); err != seed.ErrNothingToSeed {
		return expectf("Mint with only a horizon-seed tag in force = %v, want ErrNothingToSeed", err)
	}
	return nil
}

// TestHorizonSeedBindingUpgrade: a store holding a seed minted before seeds
// carried a binding card, as v0.15.0 minted them, keeps its structural
// horizon through the first pull from an origin that binds its seeds: the
// pull offers the old seed, the origin sends its bound one, dated the same,
// and the store holds it. Applied on its own, by a rebuild, the old seed is
// discarded as unbound, and the next pull fetches a bound one.
func TestHorizonSeedBindingUpgrade(t *testing.T) {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		t.Fatal(err)
	}
	bound := mustHeld(t, r)
	_, legacy, err := hzRebound(func(cards []deck.TagCard, _ *hzStruct) []deck.TagCard {
		return slices.DeleteFunc(cards, func(tc deck.TagCard) bool { return tc.Name == seed.BindingTag })
	})(x)
	if err != nil {
		t.Fatal(err)
	}
	plant := func() {
		t.Helper()
		if _, err := r.DB().Exec("UPDATE horizonseed SET seed=?", legacy); err != nil {
			t.Fatal(err)
		}
	}
	plant()
	pulled, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true})
	if err != nil || pulled.HorizonFallback {
		t.Fatalf("pull over a legacy seed: %v, fell back %v", err, pulled != nil && pulled.HorizonFallback)
	}
	if got := mustHeld(t, r); !bytes.Equal(got, bound) {
		t.Fatalf("after the pull the store holds:\n%s\nwant the origin's bound seed:\n%s", got, bound)
	}
	plant()
	if _, err := verify.Rebuild(r); err != nil {
		t.Fatal(err)
	}
	if held, _ := seed.Held(r.DB()); held != nil {
		t.Fatalf("a rebuild kept an unbound seed:\n%s", held)
	}
	if _, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true}); err != nil {
		t.Fatal(err)
	}
	if got := mustHeld(t, r); !bytes.Equal(got, bound) {
		t.Fatalf("the pull after the rebuild holds:\n%s", got)
	}
	if err := x.s.derivesDense(t, r); err != nil {
		t.Fatal(err)
	}
}
