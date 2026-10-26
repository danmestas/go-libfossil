package conformance

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// The pragmas that ride beside horizon-name (§6.1, §6.2.7), and the corners
// of name resolution: a tie in event mtime, and a name shaped like a full
// hash.

var (
	horizonNamedCursorCone = Case{ID: "horizon.6.1.named-cursor-cone", Req: "HZ-6.1-2"}
	horizonNamedPathClone  = Case{ID: "horizon.6.1.named-path-cone-clone", Req: "HZ-6.1-2"}
	horizonNamedTie        = Case{ID: "horizon.6.2.7.mtime-tie", Req: "HZ-6.2-17"}
	horizonNamedHexName    = Case{ID: "horizon.6.2.7.hash-shaped-name", Req: "HZ-6.2-17"}
)

func horizonNamedMoreCases() []Case {
	return []Case{
		horizonNamedCursorCone, horizonNamedPathClone, horizonNamedTie, horizonNamedHexName,
	}
}

func TestHorizonNamesMore(t *testing.T) {
	check(t, horizonNamedCursorCone, func(t *testing.T) error { return hzNamedCursorCone(t) })
	check(t, horizonNamedPathClone, func(t *testing.T) error { return hzNamedPathClone(t) })
	check(t, horizonNamedTie, func(t *testing.T) error { return hzNamedTie(t) })
	check(t, horizonNamedHexName, func(t *testing.T) error { return hzNamedHexName(t) })
}

// hzNamedAsk sends origin one pull through its handler, at a fixed clock,
// carrying horizon (a horizon or horizon-name pragma), pragma cone src when
// paths is set, and pragma horizon-cursor when cursor is not hzNoCursor. It
// returns the reply as the wire delivers it.
func hzNamedAsk(origin *repo.Repo, horizon xfer.Card, paths bool,
	cursor int64) (*xfer.Message, error) {
	code, err := origin.Config("project-code")
	if err != nil {
		return nil, err
	}
	cards := []xfer.Card{&xfer.PragmaCard{Name: "client-version", Values: []string{"22800"}},
		&xfer.PullCard{ServerCode: "x", ProjectCode: code}}
	if paths {
		cards = append(cards, scSyncMember("src")...)
	}
	cards = append(cards, horizon)
	if cursor != hzNoCursor {
		cards = append(cards, &xfer.PragmaCard{Name: "horizon-cursor",
			Values: []string{strconv.FormatInt(cursor, 10)}})
	}
	req, err := scSyncWire(&xfer.Message{Cards: cards})
	if err != nil {
		return nil, err
	}
	clock := simio.NewSimClockAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	opts := sync.HandleOpts{Clock: clock}
	resp, err := sync.HandleSyncWithOpts(context.Background(), origin, req, opts)
	if err != nil {
		return nil, err
	}
	return scSyncWire(resp)
}

// hzWithout is m's cards as the wire spells them, one per entry, less its
// pragmas named name.
func hzWithout(m *xfer.Message, name string) ([]string, error) {
	var out []string
	for _, card := range m.Cards {
		if p, ok := card.(*xfer.PragmaCard); ok && p.Name == name {
			continue
		}
		var b bytes.Buffer
		if err := xfer.EncodeCard(&b, card); err != nil {
			return nil, err
		}
		out = append(out, b.String())
	}
	return out, nil
}

// hzSameCards fails at the first card where got and want differ.
func hzSameCards(what string, got, want []string) error {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return expectf("%s: card %d is %.80q, want %.80q", what, i, got[i], want[i])
		}
	}
	if len(got) != len(want) {
		return expectf("%s: %d cards, want %d", what, len(got), len(want))
	}
	return nil
}

// hzNamedCursorCone: a pull naming its horizon, with a path cone and a
// cursor beside the name, is answered byte for byte as the same pull naming
// the resolved check-in by hash, less horizon-resolved. Both pragmas act:
// the cone keeps an out-of-cone file above the horizon from being announced,
// and the cursor holds back everything at or below it.
func hzNamedCursorCone(t *testing.T) error {
	g := newHZSyncGraph(t)
	r := g.f.r
	c4 := hzUUID(t, r, g.c4)
	byHash := &xfer.PragmaCard{Name: "horizon", Values: []string{c4, "descendants"}}
	first, err := hzNamedAsk(r, byHash, false, hzNoCursor)
	if err != nil {
		return err
	}
	n, err := hzCursorOf(first)
	if err != nil {
		return err
	}
	hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil) // trunk: c5 -> m -> c4
	hzCommit(t, g.f, "feat2", []libfossil.FslID{g.feat}, nil)
	byName := &xfer.PragmaCard{Name: "horizon-name", Values: []string{"trunk", "descendants", "2"}}
	named, err := hzNamedAsk(r, byName, true, n)
	if err != nil {
		return err
	}
	if err := hzResolvedFirst(named, c4, 2); err != nil {
		return err
	}
	hashed, err := hzNamedAsk(r, byHash, true, n)
	if err != nil {
		return err
	}
	got, err := hzWithout(named, "horizon-resolved")
	if err != nil {
		return err
	}
	want, err := hzWithout(hashed, "horizon-resolved")
	if err != nil {
		return err
	}
	if len(hzPragmas(hashed, "horizon-resolved")) != 0 {
		return expectf("the pull by hash was answered horizon-resolved")
	}
	if err := hzSameCards("the named pull beside the pull by hash", got, want); err != nil {
		return err
	}
	if len(hzPragmas(named, "cone-served")) != 1 {
		return expectf("the named pull's path cone was not acknowledged")
	}
	return hzNamedPragmasActed(r, byName, named, n)
}

// hzNamedPragmasActed checks that the cone and the cursor beside byName
// changed what reply announced: without the cone it announces c5's file,
// outside src; without the cursor it announces more, of which reply
// announces exactly what lies above n.
func hzNamedPragmasActed(r *repo.Repo, byName xfer.Card, reply *xfer.Message, n int64) error {
	noCone, err := hzNamedAsk(r, byName, false, n)
	if err != nil {
		return err
	}
	c5File := hash.SHA1([]byte("c5\n"))
	if !hzIGots(noCone)[c5File] || hzIGots(reply)[c5File] {
		return expectf("c5's file announced %v without the cone, %v with it; want true, false",
			hzIGots(noCone)[c5File], hzIGots(reply)[c5File])
	}
	noCursor, err := hzNamedAsk(r, byName, true, hzNoCursor)
	if err != nil {
		return err
	}
	above, err := hzAbove(r, hzIGots(noCursor), n)
	if err != nil {
		return err
	}
	if len(above) == 0 || len(above) == len(hzIGots(noCursor)) {
		return expectf("fixture: %d of %d announced artifacts lie above cursor %d",
			len(above), len(hzIGots(noCursor)), n)
	}
	return hzSameNames("announced beside horizon-name above the cursor", hzIGots(reply), above)
}

// hzNamedPathClone: a named clone with path members sends its path-cone
// pragmas beside horizon-name in the first request and beside pragma
// horizon in every later one; the origin serves both, and the clone records
// the resolved hash with its paths and holds what a clone by hash at that
// check-in with the same paths holds.
func hzNamedPathClone(t *testing.T) error {
	g := newHZSyncGraph(t)
	paths, err := cone.New([]string{"src"}, nil)
	if err != nil {
		return err
	}
	peer := &scSyncPeer{r: g.f.r}
	named := &sync.NamedHorizon{Name: "trunk", Depth: 1, Rule: cone.RuleDescendants,
		Level: cone.DepthContent}
	_, r, result, err := hzNamedCloneOpts(t, peer,
		sync.CloneOpts{StoreCone: paths, PullPhantoms: true, Named: named})
	if err != nil {
		return err
	}
	c4 := hzUUID(t, g.f.r, g.c4)
	if err := hzNamedWire(peer.requests, peer.replies, "trunk", 1, c4, 1); err != nil {
		return err
	}
	for i, req := range peer.requests {
		if got := hzPragmas(req, "cone"); len(got) != 1 || !slices.Equal(got[0], []string{"src"}) {
			return expectf("request %d path cone %q, want src", i, got)
		}
	}
	acked := len(hzPragmas(peer.replies[0], "cone-served")) == 1
	if !acked || result.ConeServed == 0 || result.HorizonServed == 0 {
		return expectf("the origin did not serve both cones: %+v", result)
	}
	if err := hzStoredHorizon(r, c4, cone.DepthContent); err != nil {
		return err
	}
	stored, err := content.StoreCone(r.DB())
	if err != nil || !slices.Equal(stored.Include(), []string{"src"}) {
		return expectf("stored paths %v (%v), want src", stored, err)
	}
	if hzHeldReal(r, hash.SHA1([]byte("m\n"))) {
		return expectf("the clone holds m's file, outside src")
	}
	sc := hzStoreCone(t, c4, cone.RuleDescendants, cone.DepthContent, "src")
	byHash, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "hash.fossil"),
		&scSyncPeer{r: g.f.r}, sync.CloneOpts{StoreCone: sc, PullPhantoms: true})
	if err != nil {
		return err
	}
	return hzSameStore(r, byHash)
}

// hzNamedTie: check-ins sharing the latest event mtime are told apart by the
// lowest full hash, for tip and for a branch name alike, never by row
// number. The fixture commits siblings at one instant until the lowest hash
// is neither the first nor the last row.
func hzNamedTie(t *testing.T) error {
	g := newHZSyncGraph(t)
	when := g.f.when.Add(time.Hour)
	var rids []libfossil.FslID
	var hashes []string
	low := 0
	for i := 0; i < 64 && (len(rids) < 3 || low == 0 || low == len(rids)-1); i++ {
		text := []byte("sibling " + strconv.Itoa(i) + "\n")
		rid, _, err := manifest.Checkin(g.f.r, manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "tie", Content: text}},
			Comment: "sibling", User: "u", Time: when, Parent: g.m, Tags: hzBranch("tie"),
		})
		if err != nil {
			return err
		}
		rids, hashes = append(rids, rid), append(hashes, hzUUID(t, g.f.r, rid))
		low = slices.Index(hashes, slices.Min(hashes))
	}
	if low == 0 || low == len(rids)-1 {
		return expectf("fixture: the lowest of %d hashes is at row %d", len(rids), low)
	}
	for _, name := range []string{"tip", "tie"} {
		if err := hzNamed(t, g.f.r, name, 0, rids[low], 0); err != nil {
			return expectf("%s: %v", name, err)
		}
	}
	return nil
}

// hzNamedHexName: a well-formed full hash, of 40 or of 64 digits, that no
// check-in has is tried as a symbolic name; one a check-in has names that
// check-in, though another carries it as a symbolic name.
func hzNamedHexName(t *testing.T) error {
	g := newHZSyncGraph(t)
	r := g.f.r
	hex40, hex64, c4 := strings.Repeat("ab", 20), strings.Repeat("cd", 32), hzUUID(t, r, g.c4)
	for i, name := range []string{hex40, hex64, c4} {
		when := g.f.when.Add(time.Duration(i+1) * time.Hour)
		if _, err := tag.AddTag(r, tag.TagOpts{TargetRID: g.c2, TagName: "sym-" + name,
			TagType: tag.TagSingleton, User: "u", Time: when}); err != nil {
			return err
		}
	}
	for _, tc := range []struct {
		name  string
		depth uint32
		want  libfossil.FslID
	}{{hex40, 0, g.c2}, {hex64, 1, g.c1}, {c4, 0, g.c4}} {
		if err := hzNamed(t, r, tc.name, tc.depth, tc.want, tc.depth); err != nil {
			return expectf("%.12s: %v", tc.name, err)
		}
	}
	return nil
}
