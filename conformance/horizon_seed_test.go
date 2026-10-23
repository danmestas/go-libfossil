package conformance

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// The seed cases (§5.2) run over hzSeed: trunk c1..c4 with C = c3, and a
// branch feat forked at c1 whose feat2 merges C in, so K = {c3, c4, feat2}
// and its entries are c3 and feat2. Below C a propagating hue is set on c1
// and feat is closed by a propagating closed tag on feat1; sym-v1 is set on
// C itself. internal/horizon/seed mints the seed at an origin and holds and
// applies it at a clone; the wire (§6.2.3) is not yet implemented, so a
// clone here is a fresh store given K's manifests and every other artifact
// but C's ancestry.

func horizonSeedCases() []Case {
	return []Case{horizonSeedCards, horizonSeedDate, horizonSeedPrivate, horizonSeedOrdinary, horizonSeedProvenance, horizonSeedNone}
}

var (
	horizonSeedCards      = Case{ID: "horizon.5.2.1.seed-cards", Req: "HZ-5.2-1"}
	horizonSeedDate       = Case{ID: "horizon.5.2.2.seed-date", Req: "HZ-5.2-2"}
	horizonSeedPrivate    = Case{ID: "horizon.5.2.3.seed-private", Req: "HZ-5.2-3"}
	horizonSeedOrdinary   = Case{ID: "horizon.5.2.3.seed-shaped-ordinary", Req: "HZ-5.2-4"}
	horizonSeedProvenance = Case{ID: "horizon.5.2.4.seed-provenance", Req: "HZ-5.2-6"}
	horizonSeedNone       = Case{ID: "horizon.5.2.1.no-tag-no-seed", Req: "HZ-5.2-7"}
)

type hzSeed struct {
	f                            *scRepo
	c1, c2, c3, c4, feat1, feat2 libfossil.FslID
	k                            []libfossil.FslID
}

func newHZSeed(t *testing.T) *hzSeed {
	t.Helper()
	s := &hzSeed{f: newSCSyncRepo(t, scSyncFiles())}
	s.c1 = s.f.c1
	s.feat1 = hzCommit(t, s.f, "feat1", []libfossil.FslID{s.c1}, hzBranch("feat"))
	s.c2 = hzCommit(t, s.f, "c2", []libfossil.FslID{s.c1}, nil)
	s.tag(t, s.c1, "hue", tag.TagPropagating, "red")
	s.c3 = hzCommit(t, s.f, "c3", []libfossil.FslID{s.c2}, nil)
	s.tag(t, s.c3, "sym-v1", tag.TagSingleton, "")
	s.c4 = hzCommit(t, s.f, "c4", []libfossil.FslID{s.c3}, nil)
	s.feat2 = hzCommit(t, s.f, "feat2", []libfossil.FslID{s.feat1, s.c3}, nil)
	s.tag(t, s.feat1, "closed", tag.TagPropagating, "")
	k, err := horizon.HeldSet(context.Background(), s.f.r.DB(), s.c3, cone.RuleDescendants)
	if err != nil {
		t.Fatal(err)
	}
	s.k = k
	return s
}

// tag applies a tag an hour after the last event, as a control artifact.
func (s *hzSeed) tag(t *testing.T, target libfossil.FslID, name string, typ int, value string) {
	t.Helper()
	s.f.when = s.f.when.Add(time.Hour)
	if _, err := tag.AddTag(s.f.r, tag.TagOpts{
		TargetRID: target, TagName: name, TagType: typ, Value: value, User: "u", Time: s.f.when,
	}); err != nil {
		t.Fatal(err)
	}
}

func (s *hzSeed) mint(login string) (string, []byte, error) {
	return seed.Mint(s.f.r.DB(), s.c3, s.k, login)
}

// hzTagState is the tag state in force at a check-in: its rows of positive
// type as name, type and value, sorted.
func hzTagState(q db.Querier, uuid string) ([]string, error) {
	rows, err := q.Query(`SELECT tag.tagname, tagxref.tagtype, coalesce(tagxref.value, '')
		FROM tagxref JOIN tag USING(tagid) JOIN blob ON blob.rid=tagxref.rid
		WHERE blob.uuid=? AND tagxref.tagtype>0`, uuid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, value string
		var typ int
		if err := rows.Scan(&name, &typ, &value); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s/%d=%s", name, typ, value))
	}
	slices.Sort(out)
	return out, rows.Err()
}

// derivesDense is nil when every check-in of K carries in clone the tag
// state and leaf verdict it carries at the origin.
func (s *hzSeed) derivesDense(t *testing.T, clone *repo.Repo) error {
	for _, rid := range s.k {
		u := hzUUID(t, s.f.r, rid)
		want, err := hzTagState(s.f.r.DB(), u)
		if err != nil {
			return err
		}
		got, err := hzTagState(clone.DB(), u)
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			return expectf("check-in %.10s carries %v, dense %v", u, got, want)
		}
		var leafGot, leafWant int
		clone.DB().QueryRow("SELECT count(*) FROM leaf JOIN blob USING(rid) WHERE uuid=?", u).Scan(&leafGot)
		s.f.r.DB().QueryRow("SELECT count(*) FROM leaf JOIN blob USING(rid) WHERE uuid=?", u).Scan(&leafWant)
		if leafGot != leafWant {
			return expectf("check-in %.10s leaf %d, dense %d", u, leafGot, leafWant)
		}
	}
	return nil
}

// lacking is a fresh store holding every artifact of the origin but the
// check-in manifests outside K, crosslinked: what a structural-horizon
// clone holds before its seed.
func (s *hzSeed) lacking(t *testing.T) *repo.Repo {
	t.Helper()
	clone, err := repo.Create(filepath.Join(t.TempDir(), "lacking.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clone.Close() })
	// The origin names its artifacts by SHA1; so must the copy.
	if err := clone.SetHashPolicy("sha1"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.f.r.DB().Query(`SELECT rid FROM blob WHERE size>=0
		AND rid NOT IN (SELECT objid FROM event WHERE type='ci') ORDER BY rid`)
	if err != nil {
		t.Fatal(err)
	}
	rids := slices.Clone(s.k)
	for rows.Next() {
		var rid libfossil.FslID
		rows.Scan(&rid)
		rids = append(rids, rid)
	}
	rows.Close()
	err = clone.WithTx(func(tx *db.Tx) error {
		for _, rid := range rids {
			data, err := content.Expand(s.f.r.DB(), rid)
			if err != nil {
				return err
			}
			if _, _, err := blob.Store(tx, data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func hzHold(r *repo.Repo, name string, data []byte, v seed.Verifier) error {
	return r.WithTx(func(tx *db.Tx) error { return seed.Hold(tx, name, data, v) })
}

func TestHorizonSeed(t *testing.T) {
	s := newHZSeed(t)
	check(t, horizonSeedCards, func() error { return hzSeedCards(t, s) })
	check(t, horizonSeedDate, func() error { return hzSeedDate(t) })
	check(t, horizonSeedNone, func() error { return hzSeedNone(t) })
	check(t, horizonSeedPrivate, func() error { return hzSeedPrivate(t, s) })
	check(t, horizonSeedOrdinary, func() error { return hzSeedOrdinary(t, s) })
	check(t, horizonSeedProvenance, func() error { return hzSeedProvenance(t, s) })
}

// hzSeedCards: the seed's T cards apply to each entry exactly the rows in
// force there densely, its U card is the origin's label, and a store lacking
// C's ancestry derives K densely once it holds the seed and not before.
func hzSeedCards(t *testing.T, s *hzSeed) error {
	_, data, err := s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	d, err := deck.Parse(data)
	if err != nil || d.Type != deck.Control {
		return expectf("seed parses as %v, %v; want a control artifact", d, err)
	}
	if d.U == nil || *d.U != seedLoginGo {
		return expectf("U card %v, want the origin's label", d.U)
	}
	c3, feat2 := hzUUID(t, s.f.r, s.c3), hzUUID(t, s.f.r, s.feat2)
	for _, line := range []string{
		"T *branch " + c3 + " trunk\n", "T +sym-v1 " + c3 + "\n", "T *hue " + c3 + " red\n",
		"T *branch " + feat2 + " feat\n", "T *closed " + feat2 + "\n",
	} {
		if !strings.Contains(string(data), line) {
			return expectf("seed lacks %q", line)
		}
	}
	cards := map[string][]string{}
	for _, tc := range d.T {
		typ := tag.TagSingleton
		if tc.Type == deck.TagPropagating {
			typ = tag.TagPropagating
		}
		cards[tc.UUID] = append(cards[tc.UUID], fmt.Sprintf("%s/%d=%s", tc.Name, typ, tc.Value))
	}
	if len(cards) != 2 {
		return expectf("seed targets %d check-ins, want the 2 entries", len(cards))
	}
	for _, u := range []string{c3, feat2} {
		want, err := hzTagState(s.f.r.DB(), u)
		if err != nil {
			return err
		}
		got := cards[u]
		slices.Sort(got)
		if !slices.Equal(got, want) {
			return expectf("cards at %.10s %v, dense rows %v", u, got, want)
		}
	}
	clone := s.lacking(t)
	if s.derivesDense(t, clone) == nil {
		return expectf("the store lacking C's ancestry derives densely without a seed")
	}
	if err := hzHold(clone, hash.SHA3(data), data, nil); err != nil {
		return err
	}
	return s.derivesDense(t, clone)
}

// hzSeedDate: D is the latest mtime among the rows summarized, so it is
// never earlier than any of them, re-minting over unchanged state yields the
// same name, and a later tag event moves D past the seed it replaces.
func hzSeedDate(t *testing.T) error {
	s := newHZSeed(t)
	name, data, err := s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	if err := hzSeedDated(s, data); err != nil {
		return err
	}
	again, _, err := s.mint(seedLoginGo)
	if err != nil || again != name {
		return expectf("re-mint over unchanged state named %s (%v), first %s", again, err, name)
	}
	// A tag event a quarter second past the second: the D card rounds up to
	// the next whole second, never down before the event, and is written
	// with no fraction, so every origin writes the same D.
	s.f.when = s.f.when.Add(time.Hour + 250*time.Millisecond)
	if _, err := tag.AddTag(s.f.r, tag.TagOpts{
		TargetRID: s.c3, TagName: "sym-v2", TagType: tag.TagSingleton, User: "u", Time: s.f.when,
	}); err != nil {
		return err
	}
	_, data, err = s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	if err := hzSeedDated(s, data); err != nil {
		return err
	}
	d, err := deck.Parse(data)
	if err != nil {
		return err
	}
	if want := s.f.when.Truncate(time.Second).Add(time.Second); !d.D.Equal(want) {
		return expectf("D %v after an event at %v, want %v", d.D, s.f.when, want)
	}
	// The U card is the origin's label for the minting implementation, never
	// a user's login.
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	if held, err := deck.Parse(mustHeld(t, r)); err != nil || held.U == nil || *held.U != seedLoginGo {
		return expectf("the served seed's U card %v (%v), want %q", held, err, seedLoginGo)
	}
	return nil
}

// hzSeedNone: where no tag is in force at any check-in a seed would cover, a
// control artifact would carry no T card, so the origin mints none and
// serves the content level: the reply advertises horizon-served content and
// the clone holds no seed.
func hzSeedNone(t *testing.T) error {
	f := newSCSyncRepo(t, scSyncFiles())
	c2 := hzCommit(t, f, "c2", []libfossil.FslID{f.c1}, nil)
	c3 := hzCommit(t, f, "c3", []libfossil.FslID{c2}, nil)
	// The fixture's check-ins sit on trunk; dropping the tag rows models an
	// origin whose check-ins carry none in force.
	if _, err := f.r.DB().Exec("DELETE FROM tagxref"); err != nil {
		return err
	}
	k, err := horizon.HeldSet(context.Background(), f.r.DB(), c3, cone.RuleDescendants)
	if err != nil {
		return err
	}
	if _, _, err := seed.Mint(f.r.DB(), c3, k, seedLoginGo); !errors.Is(err, seed.ErrNothingToSeed) {
		return expectf("Mint with no tag in force = %v, want ErrNothingToSeed", err)
	}
	h, err := cone.NewHorizon(hzUUID(t, f.r, c3), cone.RuleDescendants, cone.DepthStructural)
	if err != nil {
		return err
	}
	c, err := cone.NewWithHorizon(nil, nil, h)
	if err != nil {
		return err
	}
	peer := &scSyncPeer{r: f.r}
	clone, result, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "none.fossil"),
		peer, sync.CloneOpts{StoreCone: c, PullPhantoms: true})
	if err != nil {
		return err
	}
	t.Cleanup(func() { clone.Close() })
	for i, reply := range peer.replies {
		if got := hzPragmas(reply, "horizon-served"); len(got) != 1 || strings.Join(got[0], " ") != "content" {
			return expectf("reply %d advertises horizon-served %q, want content", i, got)
		}
		if got := hzPragmas(reply, "horizon-seed"); len(got) != 0 {
			return expectf("reply %d announces a seed %q", i, got)
		}
	}
	if held, err := seed.Held(clone.DB()); err != nil || held != nil {
		return expectf("clone holds a seed (%v)", err)
	}
	if result.StructuralServed != 0 || result.HorizonServed != result.Rounds {
		return expectf("structural %d, served %d of %d rounds", result.StructuralServed, result.HorizonServed, result.Rounds)
	}
	return nil
}

// hzSeedDated checks a seed's D card against §5.2.2: the latest mtime among
// the tag events it summarizes, at C and feat2, rounded up to a whole second
// and written with no fractional part.
func hzSeedDated(s *hzSeed, data []byte) error {
	d, err := deck.Parse(data)
	if err != nil {
		return err
	}
	var latest float64
	for _, rid := range []libfossil.FslID{s.c3, s.feat2} {
		var m float64
		s.f.r.DB().QueryRow("SELECT max(mtime) FROM tagxref WHERE rid=? AND tagtype>0", rid).Scan(&m)
		latest = max(latest, m)
	}
	ms := libfossil.MillisFromJulianDay(latest)
	if want := (ms + 999) / 1000 * 1000; d.D.UnixMilli() != want {
		return expectf("D %v, want the latest summarized event %v rounded up to a second",
			d.D, time.UnixMilli(ms).UTC())
	}
	line, _, _ := strings.Cut(string(data), "\n")
	if !strings.HasPrefix(line, "D ") || strings.Contains(line, ".") {
		return expectf("D card %q, want whole seconds with no fraction", line)
	}
	return nil
}

// hzSeedPrivate: the held seed is no artifact of the store. Holding it adds
// no blob and marks nothing to send, a peer asking for it by name or cloning
// the store is never sent it, a rebuild re-applies it without making it a
// blob, and the derivation keeps it.
func hzSeedPrivate(t *testing.T, s *hzSeed) error {
	clone := s.lacking(t)
	if _, err := clone.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		return err
	}
	counts := func() string {
		var b strings.Builder
		for _, table := range []string{"blob", "unsent", "unclustered", "private", "phantom"} {
			var n int
			clone.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n)
			fmt.Fprintf(&b, "%s=%d ", table, n)
		}
		return b.String()
	}
	before := counts()
	name, data, err := s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	if err := hzHold(clone, name, data, nil); err != nil {
		return err
	}
	if after := counts(); after != before {
		return expectf("holding the seed changed the store: %s, was %s", after, before)
	}
	var pc string
	clone.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&pc)
	for _, req := range [][]xfer.Card{
		{&xfer.PullCard{ServerCode: "x", ProjectCode: pc}, &xfer.GimmeCard{UUID: name}},
		{&xfer.CloneCard{Version: 3, SeqNo: 1}},
	} {
		resp, err := sync.HandleSync(context.Background(), clone, &xfer.Message{Cards: append([]xfer.Card{
			&xfer.PragmaCard{Name: "client-version", Values: []string{"22800", "20260101", "000000"}},
		}, req...)})
		if err != nil {
			return err
		}
		wire, err := resp.EncodeUncompressed()
		if err != nil {
			return err
		}
		if strings.Contains(string(wire), name) {
			return expectf("a reply to %T names the seed", req[len(req)-1])
		}
	}
	if _, err := verify.Rebuild(clone); err != nil {
		return err
	}
	var n int
	clone.DB().QueryRow("SELECT count(*) FROM blob WHERE uuid=?", name).Scan(&n)
	if n != 0 {
		return expectf("the rebuild stored the seed as a blob")
	}
	return s.derivesDense(t, clone)
}

// hzSeedOrdinary: an artifact of the seed's shape arriving as an ordinary
// artifact is stored and crosslinked as the control artifact it is, offered
// to peers like any other, and never held as the seed.
func hzSeedOrdinary(t *testing.T, s *hzSeed) error {
	clone := s.lacking(t)
	name, data, err := s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	var rid libfossil.FslID
	err = clone.WithTx(func(tx *db.Tx) error {
		var err error
		rid, _, err = blob.Store(tx, data)
		return err
	})
	if err != nil {
		return err
	}
	if _, err := manifest.Crosslink(clone); err != nil {
		return err
	}
	var typ string
	var sources, unclustered int
	clone.DB().QueryRow("SELECT type FROM event WHERE objid=?", rid).Scan(&typ)
	clone.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", rid).Scan(&sources)
	clone.DB().QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid).Scan(&unclustered)
	if typ != "g" || sources == 0 || unclustered != 1 {
		return expectf("artifact %.10s: event %q, %d tag rows sourced, unclustered %d; want an ordinary control artifact",
			name, typ, sources, unclustered)
	}
	held, err := seed.Held(clone.DB())
	if err != nil || held != nil {
		return expectf("an ordinary artifact was held as the seed (%v)", err)
	}
	return nil
}

// hzSeedProvenance: a clone with no key for its origin holds an unsigned
// seed; one that knows the key holds no seed the key does not verify, and
// discards a held seed that stops verifying, falling back to the content
// horizon's derivation.
func hzSeedProvenance(t *testing.T, s *hzSeed) error {
	clone := s.lacking(t)
	name, data, err := s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	unsigned := errors.New("no signature by the origin's key")
	reject := func([]byte) error { return unsigned }
	if err := hzHold(clone, name, data, reject); !errors.Is(err, seed.ErrProvenance) {
		return expectf("Hold under a rejecting key: %v, want ErrProvenance", err)
	}
	if held, _ := seed.Held(clone.DB()); held != nil {
		return expectf("a seed the key rejected is held")
	}
	if err := hzHold(clone, name, data, nil); err != nil {
		return expectf("Hold with no key refused an unsigned seed: %v", err)
	}
	var applied bool
	err = clone.WithTx(func(tx *db.Tx) error {
		var err error
		applied, err = seed.Apply(tx, reject)
		return err
	})
	if err != nil {
		return err
	}
	if held, _ := seed.Held(clone.DB()); applied || held != nil {
		return expectf("a held seed the key rejected was applied (%v) or kept", applied)
	}
	if s.derivesDense(t, clone) == nil {
		return expectf("the discarded seed's state survived")
	}
	// A verifier that could not judge the seed (gpg absent, say) is no
	// verdict: Hold refuses without ErrProvenance, so the caller does not
	// fall back, and Apply keeps a held seed and reports the fault.
	cannot := func([]byte) error { return clearsign.ErrCannotVerify }
	if err := hzHold(clone, name, data, cannot); err == nil || errors.Is(err, seed.ErrProvenance) {
		return expectf("Hold when the key cannot be checked: %v, want an error that is not ErrProvenance", err)
	}
	if err := hzHold(clone, name, data, nil); err != nil {
		return err
	}
	err = clone.WithTx(func(tx *db.Tx) error {
		_, err := seed.Apply(tx, cannot)
		return err
	})
	if !errors.Is(err, clearsign.ErrCannotVerify) {
		return expectf("Apply when the key cannot be checked: %v, want ErrCannotVerify", err)
	}
	if held, _ := seed.Held(clone.DB()); held == nil {
		return expectf("a seed was discarded because its key could not be checked")
	}
	return nil
}

// TestHorizonSeedZit has zit 0.6.4 mint a seed for the same repository and
// compares it with go-libfossil's: go-libfossil serves hzSeed, zit clones it
// densely and serves that clone, and a second zit clone at C's structural
// horizon receives the seed. Every tag event in hzSeed is on a whole second,
// so with zit's login the two seeds must be the same bytes and so the same
// name; each implementation must then derive from the other's seed what it
// derives from its own.
func TestHorizonSeedZit(t *testing.T) {
	bin := zitHorizonBinary(t)
	s := newHZSeed(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(xferHandler(t, s.f.r, func(*xfer.Message) {}))
	defer srv.Close()
	dense := filepath.Join(dir, "dense")
	zitRun(t, bin, home, dir, "clone", srv.URL, dense, "--no-open", "--once")
	url := zitServe(t, bin, home, dense)
	c := hzUUID(t, s.f.r, s.c3)
	structural := filepath.Join(dir, "structural")
	zitRun(t, bin, home, dir, "clone", url, structural, "--no-open", "--once", "--horizon="+c, "--structural")
	zitSeed, err := os.ReadFile(filepath.Join(structural, ".zit-cache", "horizon-seed"))
	if err != nil {
		t.Fatalf("zit held no seed: %v", err)
	}
	goName, goSeed, err := s.mint("zit-serve")
	if err != nil {
		t.Fatal(err)
	}
	if string(goSeed) != string(zitSeed) || goName != hash.SHA3(zitSeed) {
		t.Errorf("seeds differ:\nzit %s\n%s\ngo  %s\n%s", hash.SHA3(zitSeed), zitSeed, goName, goSeed)
	}
	// zit derives from go-libfossil's seed what it derives from its own:
	// feat2's branch and closed leaf, and the branch list. Without a seed
	// it derives neither, so the comparison shows the seed applied.
	sidecar := filepath.Join(structural, ".zit-cache", "horizon-seed")
	feat2 := hzUUID(t, s.f.r, s.feat2)
	derived := func() string {
		return zitRun(t, bin, home, structural, "info", feat2) + zitRun(t, bin, home, structural, "branch")
	}
	own := derived()
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if none := derived(); none == own {
		t.Fatalf("zit derives the same state without its seed:\n%s", none)
	}
	if err := os.WriteFile(sidecar, goSeed, 0o644); err != nil {
		t.Fatal(err)
	}
	if fromGo := derived(); fromGo != own {
		t.Errorf("zit from go-libfossil's seed:\n%s\nfrom its own:\n%s", fromGo, own)
	}
	clone := s.lacking(t)
	if err := hzHold(clone, hash.SHA3(zitSeed), zitSeed, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.derivesDense(t, clone); err != nil {
		t.Errorf("zit's seed applied by go-libfossil: %v", err)
	}
}
