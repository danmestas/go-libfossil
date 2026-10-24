package conformance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// Named horizons (draft-fossil-horizon-00 §6.1, §6.2.7, §6.3 as zit 0.7.1's
// erratum amends them): a clone asks for its horizon by name and depth in
// its first request, the origin resolves the name over its dense repository
// and echoes the check-in, and every later request names that check-in by
// hash. Each case is checked at both ends: go-libfossil's handler as the
// origin, from the wire it answers with, and go-libfossil's client, from the
// store it leaves.

var (
	horizonNamedTip        = Case{ID: "horizon.6.2.7.tip", Req: "HZ-6.2-17"}
	horizonNamedBranch     = Case{ID: "horizon.6.2.7.branch-depth", Req: "HZ-6.2-17"}
	horizonNamedTag        = Case{ID: "horizon.6.2.7.tag", Req: "HZ-6.2-17"}
	horizonNamedHash       = Case{ID: "horizon.6.2.7.hash-depth", Req: "HZ-6.2-17"}
	horizonNamedStructural = Case{ID: "horizon.6.2.7.structural-seed", Req: "HZ-6.2-17"}
	horizonNamedUnknown    = Case{ID: "horizon.6.2.7.unknown-name", Req: "HZ-6.2-18"}
	horizonNamedMalformed  = Case{ID: "horizon.6.2.7.malformed", Req: "HZ-6.2-18"}
	horizonNamedPastRoot   = Case{ID: "horizon.6.3.named-past-root", Req: "HZ-6.3-3"}
	horizonNamedChecked    = Case{ID: "horizon.6.3.named-resolved-checked", Req: "HZ-6.3-3"}
	horizonNamedFallback   = Case{ID: "horizon.6.3.named-fallback", Req: "HZ-6.3-4"}
	horizonNamedFossil     = Case{ID: "horizon.6.3.named-fossil-fallback", Req: "HZ-6.3-4"}
	horizonNamedLaterPull  = Case{ID: "horizon.6.1.named-later-pull", Req: "HZ-6.1-2"}
	horizonNamedNoOracle   = Case{ID: "horizon.6.2.7.no-name-oracle", Req: "HZ-6.2-18"}
	horizonNamedPullName   = Case{ID: "horizon.6.2.7.pull-unknown-name", Req: "HZ-6.2-18"}
	horizonNamedLogin      = Case{ID: "horizon.6.2.7.login-retry", Req: "HZ-6.1-2"}
)

func horizonNamesCases() []Case {
	return []Case{
		horizonNamedTip, horizonNamedBranch, horizonNamedTag, horizonNamedHash, horizonNamedStructural,
		horizonNamedUnknown, horizonNamedMalformed, horizonNamedPastRoot, horizonNamedChecked,
		horizonNamedFallback, horizonNamedFossil, horizonNamedLaterPull,
		horizonNamedNoOracle, horizonNamedPullName, horizonNamedLogin,
	}
}

func TestHorizonNames(t *testing.T) {
	check(t, horizonNamedTip, func() error {
		g := newHZSyncGraph(t)
		return hzNamed(t, g.f.r, "tip", 0, g.side2, 0)
	})
	check(t, horizonNamedBranch, func() error {
		g := newHZSyncGraph(t)
		return hzNamed(t, g.f.r, "side", 3, g.c2, 3) // side2 -> side1 -> c3 -> c2
	})
	check(t, horizonNamedTag, func() error {
		g := newHZSyncGraph(t)
		g.f.when = g.f.when.Add(time.Hour)
		if _, err := tag.AddTag(g.f.r, tag.TagOpts{TargetRID: g.c4, TagName: "sym-rel",
			TagType: tag.TagSingleton, User: "u", Time: g.f.when}); err != nil {
			return err
		}
		return hzNamed(t, g.f.r, "rel", 0, g.c4, 0)
	})
	check(t, horizonNamedHash, func() error {
		g := newHZSyncGraph(t)
		return hzNamed(t, g.f.r, hzUUID(t, g.f.r, g.m), 2, g.c3, 2) // m -> c4 -> c3
	})
	check(t, horizonNamedPastRoot, func() error {
		g := newHZSyncGraph(t)
		return hzNamed(t, g.f.r, "feat", 4, g.c1, 1) // feat -> c1, the root
	})
	check(t, horizonNamedStructural, func() error { return hzNamedStructural(t) })
	check(t, horizonNamedUnknown, func() error { return hzNamedUnknown(t) })
	check(t, horizonNamedMalformed, func() error { return hzNamedMalformed(t) })
	check(t, horizonNamedChecked, func() error { return hzNamedChecked(t) })
	check(t, horizonNamedFallback, func() error { return hzNamedFallback(t, false) })
	check(t, horizonNamedFossil, func() error { return hzNamedFallback(t, true) })
	check(t, horizonNamedLaterPull, func() error { return hzNamedLaterPull(t) })
	check(t, horizonNamedNoOracle, func() error { return hzNamedNoOracle(t) })
	check(t, horizonNamedPullName, func() error { return hzNamedPullName(t) })
	check(t, horizonNamedLogin, func() error { return hzNamedLogin(t) })
}

// hzNamedClone clones origin through peer asking for the horizon by name
// and depth under the descendants rule at level.
func hzNamedClone(t *testing.T, peer sync.Transport, name string, depth uint32,
	level cone.HorizonDepth) (string, *repo.Repo, *sync.CloneResult, error) {
	t.Helper()
	return hzNamedCloneOpts(t, peer, sync.CloneOpts{PullPhantoms: true, Named: &sync.NamedHorizon{
		Name: name, Depth: depth, Rule: cone.RuleDescendants, Level: level}})
}

func hzNamedCloneOpts(t *testing.T, peer sync.Transport, opts sync.CloneOpts) (string, *repo.Repo, *sync.CloneResult, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "named.fossil")
	r, result, err := sync.Clone(context.Background(), path, peer, opts)
	if err != nil {
		return path, nil, nil, err
	}
	t.Cleanup(func() { r.Close() })
	return path, r, result, nil
}

// hzNamed clones origin by name and depth and checks both ends: the origin
// echoed C and the steps it took, the client recorded C as its horizon and
// reported the steps, and the clone holds what a clone at C by hash holds.
func hzNamed(t *testing.T, origin *repo.Repo, name string, depth uint32, want libfossil.FslID, steps uint32) error {
	c := hzUUID(t, origin, want)
	peer := &scSyncPeer{r: origin}
	_, r, result, err := hzNamedClone(t, peer, name, depth, cone.DepthContent)
	if err != nil {
		return err
	}
	if err := hzNamedWire(peer.requests, peer.replies, name, depth, c, steps); err != nil {
		return err
	}
	if result.HorizonResolved != c || result.HorizonSteps != steps || result.HorizonResolvedLocally {
		return expectf("clone resolved %.12s after %d steps (local %v), want %.12s after %d by the origin",
			result.HorizonResolved, result.HorizonSteps, result.HorizonResolvedLocally, c, steps)
	}
	if err := hzStoredHorizon(r, c, cone.DepthContent); err != nil {
		return err
	}
	byHash, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "hash.fossil"),
		&scSyncPeer{r: origin}, sync.CloneOpts{StoreCone: hzStoreCone(t, c, cone.RuleDescendants, cone.DepthContent),
			PullPhantoms: true})
	if err != nil {
		return err
	}
	return hzSameStore(r, byHash)
}

// hzSameStore compares two stores' held and phantom artifacts.
func hzSameStore(a, b *repo.Repo) error {
	defer b.Close()
	for _, q := range []string{
		"SELECT uuid FROM blob WHERE size>=0 ORDER BY uuid",
		"SELECT uuid FROM blob WHERE size<0 ORDER BY uuid",
	} {
		x, err := hzColumn(a, q)
		if err != nil {
			return err
		}
		y, err := hzColumn(b, q)
		if err != nil {
			return err
		}
		if !slices.Equal(x, y) {
			return expectf("named clone %q: %d rows, clone by hash %d", q, len(x), len(y))
		}
	}
	return nil
}

func hzColumn(r *repo.Repo, q string) ([]string, error) {
	rows, err := r.DB().Query(q)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, s)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// hzNamedWire checks a named clone's transcript: its first request asks for
// name by horizon-name and carries no horizon pragma; the first reply
// echoes c and steps ahead of horizon-served and of every file card; every
// later request names c by pragma horizon and never the name.
func hzNamedWire(requests, replies []*xfer.Message, name string, depth uint32, c string, steps uint32) error {
	if len(requests) < 2 || len(replies) == 0 {
		return expectf("%d requests and %d replies, want a clone's", len(requests), len(replies))
	}
	want := []string{deck.FossilEncode(name), "descendants", strconv.FormatUint(uint64(depth), 10)}
	if got := hzPragmas(requests[0], "horizon-name"); len(got) != 1 || !slices.Equal(got[0], want) {
		return expectf("first request horizon-name %q, want %q", got, want)
	}
	if got := hzPragmas(requests[0], "horizon"); len(got) != 0 {
		return expectf("first request also carries pragma horizon %q", got)
	}
	if err := hzResolvedFirst(replies[0], c, steps); err != nil {
		return err
	}
	for i, req := range requests[1:] {
		if got := hzPragmas(req, "horizon-name"); len(got) != 0 {
			return expectf("request %d sends the name again: %q", i+1, got)
		}
		if got := hzPragmas(req, "horizon"); len(got) != 1 || got[0][0] != c {
			return expectf("request %d horizon %q, want %.12s", i+1, got, c)
		}
	}
	return nil
}

// hzResolvedFirst checks a reply carries pragma horizon-resolved c steps,
// once, ahead of horizon-served and every file card.
func hzResolvedFirst(m *xfer.Message, c string, steps uint32) error {
	at, served, file := -1, -1, -1
	for i, card := range m.Cards {
		switch x := card.(type) {
		case *xfer.PragmaCard:
			if x.Name == "horizon-resolved" {
				if at >= 0 || !slices.Equal(x.Values, []string{c, strconv.FormatUint(uint64(steps), 10)}) {
					return expectf("horizon-resolved %q, want one naming %.12s after %d steps", x.Values, c, steps)
				}
				at = i
			}
			if x.Name == "horizon-served" && served < 0 {
				served = i
			}
		case *xfer.FileCard, *xfer.CFileCard:
			if file < 0 {
				file = i
			}
		}
	}
	if at < 0 || served < at || file >= 0 && file < at {
		return expectf("horizon-resolved at %d, horizon-served at %d, first file at %d", at, served, file)
	}
	return nil
}

// A structural clone by name gets the seed minted for the resolved C: the
// trunk's tip c4 stepped back one check-in to c3, whose seed the origin
// mints byte for byte as it does for a request naming c3 by hash.
func hzNamedStructural(t *testing.T) error {
	x := newHZStruct(t)
	peer := x.peer()
	_, r, result, err := hzNamedClone(t, peer, "trunk", 1, cone.DepthStructural)
	if err != nil {
		return err
	}
	if result.HorizonResolved != x.c3 || result.HorizonFallback || result.StructuralServed == 0 {
		return expectf("structural clone by name: %+v", result)
	}
	if got := hzPragmas(peer.requests[0], "horizon-structural"); len(got) != 1 {
		return expectf("first request horizon-structural %q, want one", got)
	}
	if err := hzResolvedFirst(peer.replies[0], x.c3, 1); err != nil {
		return err
	}
	if err := hzStoredHorizon(r, x.c3, cone.DepthStructural); err != nil {
		return err
	}
	_, want, err := x.s.mint(seedLoginGo)
	if err != nil {
		return err
	}
	if got := mustHeld(t, r); string(got) != string(want) {
		return expectf("held seed differs from the one minted for c3:\n%s\nwant\n%s", got, want)
	}
	return nil
}

// An unknown name is answered with one error card and nothing else, and the
// client fails the clone leaving nothing on disk.
func hzNamedUnknown(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer := &scSyncPeer{r: g.f.r}
	path, _, _, err := hzNamedClone(t, peer, "no such", 0, cone.DepthContent)
	if err == nil || !strings.Contains(err.Error(), "horizon name not found: no such") {
		return expectf("clone of an unknown name = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		return expectf("the failed clone left %s (%v)", path, statErr)
	}
	return hzOnlyError(peer.replies[0], "horizon name not found: no such")
}

// hzOnlyError checks m is the one error card msg.
func hzOnlyError(m *xfer.Message, msg string) error {
	if len(m.Cards) != 1 {
		return expectf("reply carries %d cards, want only the error %q", len(m.Cards), msg)
	}
	if e, ok := m.Cards[0].(*xfer.ErrorCard); !ok || e.Message != msg {
		return expectf("reply %#v, want error %q", m.Cards[0], msg)
	}
	return nil
}

// A malformed depth, both pragma forms in one request, or two horizon-name
// pragmas are answered with one error card. A requester the origin refuses
// is told that first, whatever the name.
func hzNamedMalformed(t *testing.T) error {
	g := newHZSyncGraph(t)
	c := hzUUID(t, g.f.r, g.c3)
	name := func(depth string) xfer.Card {
		return &xfer.PragmaCard{Name: "horizon-name", Values: []string{"trunk", "descendants", depth}}
	}
	horizon := &xfer.PragmaCard{Name: "horizon", Values: []string{c, "descendants"}}
	for _, extra := range [][]xfer.Card{
		{name("x")}, {name("-1")}, {name("4294967296")}, {name("+1")}, {name("")},
		{name("1"), horizon}, {horizon, name("1")}, {name("1"), name("1")},
		{&xfer.PragmaCard{Name: "horizon-name", Values: []string{"trunk", "sideways", "0"}}},
		{&xfer.PragmaCard{Name: "horizon-name", Values: []string{"trunk", "descendants"}}},
	} {
		req := &xfer.Message{Cards: append(slices.Clone(extra), &xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true})}
		resp, err := sync.HandleSync(context.Background(), g.f.r, req)
		if err != nil {
			return err
		}
		if err := hzOnlyError(resp, "malformed horizon-name pragma"); err != nil {
			return expectf("%v: %v", extra, err)
		}
	}
	if _, err := g.f.r.DB().Exec("UPDATE user SET cap='' WHERE login='nobody'"); err != nil {
		return err
	}
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "horizon-name", Values: []string{"nope", "descendants", "0"}},
		&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true},
	}}
	resp, err := sync.HandleSync(context.Background(), g.f.r, req)
	if err != nil {
		return err
	}
	for _, card := range resp.Cards {
		if p, ok := card.(*xfer.PragmaCard); ok && p.Name == "horizon-resolved" {
			return expectf("a refused requester was sent %q", p.Values)
		}
		if e, ok := card.(*xfer.ErrorCard); ok && e.Message != "not authorized to clone" {
			return expectf("a refused requester was told %q", e.Message)
		}
	}
	return nil
}

// The client fails the clone, leaving nothing on disk, when horizon-resolved
// names no full hash, takes more steps than asked, arrives after a file card
// or without a horizon-name asking for it, or names a check-in the clone
// never receives.
func hzNamedChecked(t *testing.T) error {
	g := newHZSyncGraph(t)
	c := hzUUID(t, g.f.r, g.c3)
	resolved := func(values ...string) func(*xfer.Message) {
		return func(m *xfer.Message) {
			for _, card := range m.Cards {
				if p, ok := card.(*xfer.PragmaCard); ok && p.Name == "horizon-resolved" {
					p.Values = values
				}
			}
		}
	}
	twice := func(m *xfer.Message) {
		for i, card := range m.Cards {
			if p, ok := card.(*xfer.PragmaCard); ok && p.Name == "horizon-resolved" {
				m.Cards = slices.Insert(m.Cards, i+1, xfer.Card(p))
				return
			}
		}
	}
	late := func(m *xfer.Message) {
		for i, card := range m.Cards {
			if p, ok := card.(*xfer.PragmaCard); ok && p.Name == "horizon-resolved" {
				m.Cards = append(slices.Delete(m.Cards, i, i+1), p)
				return
			}
		}
	}
	for _, tc := range []struct {
		name    string
		depth   uint32
		rewrite func(*xfer.Message)
		want    string
	}{
		{"trunk", 1, resolved("c3", "1"), "horizon-resolved"},
		{"trunk", 1, resolved(c, "2"), "horizon-resolved"},
		{"trunk", 1, resolved(c), "horizon-resolved"},
		{"trunk", 1, late, "horizon-resolved"},
		{"trunk", 1, resolved(strings.Repeat("ab", 20), "1"), "never arrived"},
		{"trunk", 1, twice, "horizon-resolved"},
	} {
		peer := &hzRewritingPeer{inner: &scSyncPeer{r: g.f.r}, rewrite: tc.rewrite}
		path, _, _, err := hzNamedClone(t, peer, tc.name, tc.depth, cone.DepthContent)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			return expectf("clone = %v, want an error naming %q", err, tc.want)
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			return expectf("the failed clone left %s (%v)", path, statErr)
		}
	}
	// A clone by hash asked for no name, so a reply resolving one fails it.
	unasked := &hzRewritingPeer{inner: &scSyncPeer{r: g.f.r}, rewrite: func(m *xfer.Message) {
		m.Cards = append([]xfer.Card{&xfer.PragmaCard{Name: "horizon-resolved", Values: []string{c, "0"}}}, m.Cards...)
	}}
	_, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "hash.fossil"), unasked,
		sync.CloneOpts{StoreCone: hzStoreCone(t, c, cone.RuleDescendants, cone.DepthContent)})
	if err == nil || !strings.Contains(err.Error(), "horizon-resolved") {
		return expectf("clone by hash given horizon-resolved = %v", err)
	}
	// A reply to a request that sent no horizon-name, here the cleanup
	// request after an empty first reply, may not resolve one either.
	scripted := &hzScriptedPeer{replies: []*xfer.Message{{}, {Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "horizon-resolved", Values: []string{c, "0"}},
		&xfer.CloneSeqNoCard{SeqNo: 0},
	}}}}
	path, _, _, err := hzNamedClone(t, scripted, "trunk", 1, cone.DepthContent)
	if err == nil || !strings.Contains(err.Error(), "unexpected horizon-resolved") {
		return expectf("clone given horizon-resolved to an unnamed request = %v", err)
	}
	if got := hzPragmas(scripted.requests[1], "horizon-name"); len(got) != 0 {
		return expectf("the cleanup request sent horizon-name %q", got)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		return expectf("the failed clone left %s (%v)", path, statErr)
	}
	return nil
}

// hzScriptedPeer answers each request with the next of replies.
type hzScriptedPeer struct {
	replies  []*xfer.Message
	requests []*xfer.Message
}

func (p *hzScriptedPeer) Exchange(_ context.Context, req *xfer.Message) (*xfer.Message, error) {
	p.requests = append(p.requests, req)
	if len(p.requests) > len(p.replies) {
		return nil, expectf("request %d past the script", len(p.requests))
	}
	return p.replies[len(p.requests)-1], nil
}

// Against a peer that answers no horizon-resolved, the client finishes the
// dense transfer, resolves the name over what it holds, classifies at the
// content horizon, records the hash and says it resolved locally. A
// structural request falls back to the content horizon. Its store matches a
// clone of the same peer at the resolved check-in by hash.
func hzNamedFallback(t *testing.T, fossil bool) error {
	g := newHZSyncGraph(t)
	for _, level := range []cone.HorizonDepth{cone.DepthContent, cone.DepthStructural} {
		peer := newHZPeer(t, g.f.r, fossil)
		if peer == nil {
			return nil
		}
		_, r, result, err := hzNamedClone(t, peer, "trunk", 2, level)
		if err != nil {
			return err
		}
		c := hzUUID(t, g.f.r, g.c3) // m -> c4 -> c3
		if result.HorizonResolved != c || result.HorizonSteps != 2 || !result.HorizonResolvedLocally ||
			result.HorizonFallback != (level == cone.DepthStructural) || result.Elided == 0 {
			return expectf("%v fallback clone: %+v", level, result)
		}
		if got := hzPragmas(peer.sent()[0], "horizon-name"); len(got) != 1 {
			return expectf("first request horizon-name %q, want one", got)
		}
		if err := hzStoredHorizon(r, c, cone.DepthContent); err != nil {
			return err
		}
		byHash, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "hash.fossil"),
			newHZPeer(t, g.f.r, fossil), sync.CloneOpts{StoreCone: hzStoreCone(t, c, cone.RuleDescendants,
				cone.DepthContent), PullPhantoms: true})
		if err != nil {
			return err
		}
		if err := hzSameStore(r, byHash); err != nil {
			return err
		}
	}
	return nil
}

// A store cloned by name records only the hash: a later pull sends pragma
// horizon with the resolved check-in and never the name.
func hzNamedLaterPull(t *testing.T) error {
	g := newHZSyncGraph(t)
	peer := &scSyncPeer{r: g.f.r}
	_, r, _, err := hzNamedClone(t, peer, "trunk", 1, cone.DepthContent)
	if err != nil {
		return err
	}
	clone := len(peer.requests)
	hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	if _, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true}); err != nil {
		return err
	}
	c := hzUUID(t, g.f.r, g.c4)
	for i, req := range peer.requests[clone:] {
		if got := hzPragmas(req, "horizon-name"); len(got) != 0 {
			return expectf("pull request %d sends the name: %q", i, got)
		}
		if got := hzPragmas(req, "horizon"); len(got) != 1 || got[0][0] != c {
			return expectf("pull request %d horizon %q, want %.12s", i, got, c)
		}
	}
	return hzStoredHorizon(r, c, cone.DepthContent)
}

// hzNamedNoOracle: a requester who may neither read nor clone gets the same
// reply bytes for a name the origin holds as for one it does not, whatever
// else the request asks; neither carries horizon-resolved or horizon-served.
func hzNamedNoOracle(t *testing.T) error {
	g := newHZSyncGraph(t)
	if _, err := g.f.r.DB().Exec("UPDATE user SET cap='' WHERE login='nobody'"); err != nil {
		return err
	}
	project, err := g.f.r.Config("project-code")
	if err != nil {
		return err
	}
	clock := simio.NewSimClockAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	version := &xfer.PragmaCard{Name: "client-version", Values: []string{"22800", "20260101", "120000"}}
	for _, tail := range [][]xfer.Card{
		nil,
		{&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true}},
		{&xfer.PullCard{ServerCode: "x", ProjectCode: project}},
	} {
		var replies [2][]byte
		for i, name := range []string{"trunk", "nope"} {
			cards := append([]xfer.Card{version, &xfer.PragmaCard{Name: "horizon-name",
				Values: []string{name, "descendants", "0"}}}, tail...)
			resp, err := sync.HandleSyncWithOpts(context.Background(), g.f.r, &xfer.Message{Cards: cards},
				sync.HandleOpts{Clock: clock})
			if err != nil {
				return err
			}
			for _, card := range resp.Cards {
				if p, ok := card.(*xfer.PragmaCard); ok && strings.HasPrefix(p.Name, "horizon") {
					return expectf("%s with %d more cards: an unauthorized requester was sent pragma %s", name, len(tail), p.Name)
				}
			}
			if replies[i], err = resp.Encode(); err != nil {
				return err
			}
		}
		if string(replies[0]) != string(replies[1]) {
			return expectf("replies differ by name with %d more cards:\n%q\n%q", len(tail), replies[0], replies[1])
		}
	}
	return nil
}

// hzNamedPullName: a pull naming a horizon the origin cannot resolve is
// refused with the one error card, not served densely; one it can resolve
// is echoed and served at the horizon.
func hzNamedPullName(t *testing.T) error {
	g := newHZSyncGraph(t)
	project, err := g.f.r.Config("project-code")
	if err != nil {
		return err
	}
	for _, name := range []string{"nope", "trunk"} {
		resp, err := sync.HandleSync(context.Background(), g.f.r, &xfer.Message{Cards: []xfer.Card{
			&xfer.PragmaCard{Name: "horizon-name", Values: []string{name, "descendants", "1"}},
			&xfer.PullCard{ServerCode: "x", ProjectCode: project},
		}})
		if err != nil {
			return err
		}
		if name == "nope" {
			if err := hzOnlyError(resp, "horizon name not found: nope"); err != nil {
				return err
			}
			continue
		}
		if got := hzPragmas(resp, "horizon-resolved"); len(got) != 1 || got[0][0] != hzUUID(t, g.f.r, g.c4) {
			return expectf("pull naming trunk at depth 1 was echoed %q, want c4", got)
		}
	}
	return nil
}

// hzNamedLogin: a clone by name of a private origin is refused in round 0,
// which carries no login, and resends the name with the login in round 1:
// still the clone's first request in §6.1's sense. The origin resolves it
// then, and the clone is not left to resolve it locally.
func hzNamedLogin(t *testing.T) error {
	g := newHZSyncGraph(t)
	if _, err := g.f.r.DB().Exec("UPDATE user SET cap='' WHERE login='nobody'"); err != nil {
		return err
	}
	project, err := g.f.r.Config("project-code")
	if err != nil {
		return err
	}
	if err := auth.CreateUser(g.f.r.DB(), project, "named-reader", "test-password", "go"); err != nil {
		return err
	}
	peer := &scSyncPeer{r: g.f.r}
	_, r, result, err := hzNamedCloneOpts(t, peer, sync.CloneOpts{User: "named-reader", Password: "test-password",
		Named: &sync.NamedHorizon{Name: "trunk", Depth: 1, Rule: cone.RuleDescendants, Level: cone.DepthContent}})
	if err != nil {
		return err
	}
	c := hzUUID(t, g.f.r, g.c4)
	if result.HorizonResolved != c || result.HorizonResolvedLocally || result.HorizonServed == 0 {
		return expectf("private named clone: %+v, want c4 resolved by the origin", result)
	}
	if len(peer.requests) < 2 || len(hzPragmas(peer.requests[1], "horizon-name")) != 1 {
		return expectf("round 1 did not resend the name with the login")
	}
	if err := hzResolvedFirst(peer.replies[1], c, 1); err != nil {
		return err
	}
	return hzStoredHorizon(r, c, cone.DepthContent)
}
