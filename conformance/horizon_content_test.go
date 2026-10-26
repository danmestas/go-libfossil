package conformance

import (
	"context"
	"maps"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
)

// The content horizon of §4 over a store: a clone holding every manifest
// and only the blobs its held set names, classified by internal/content.

var (
	horizonContentAlone   = Case{ID: "horizon.1.1.content-alone", Req: "HZ-1.1-1"}
	horizonStoreRules     = Case{ID: "horizon.3.1.store-rules", Req: "HZ-3.1-1"}
	horizonStoreRecompute = Case{ID: "horizon.3.1.store-recomputed", Req: "HZ-3.1-3"}
	horizonStoreLoss      = Case{ID: "horizon.3.2.store-loss", Req: "HZ-3.2-1"}
	horizonStorePaths     = Case{ID: "horizon.3.2.paths", Req: "HZ-3.2-2"}
	horizonHoldings       = Case{ID: "horizon.4.1.holdings", Req: "HZ-4.1-1"}
	horizonAboveTree      = Case{ID: "horizon.4.2.above-tree", Req: "HZ-4.2-1"}
	horizonDeltaBase      = Case{ID: "horizon.4.2.delta-base", Req: "HZ-4.2-2"}
	horizonDerived        = Case{ID: "horizon.4.3.derived", Req: "HZ-4.3-1"}
)

func horizonContentCases() []Case {
	return []Case{
		horizonContentAlone, horizonStoreRules, horizonStoreRecompute, horizonStoreLoss,
		horizonStorePaths, horizonHoldings, horizonAboveTree, horizonDeltaBase, horizonDerived,
	}
}

func TestHorizonContent(t *testing.T) {
	check(t, horizonContentAlone, func(t *testing.T) error { return hzContentAlone(t) })
	check(t, horizonStoreRules, func(t *testing.T) error { return hzStoreRules(t) })
	check(t, horizonStoreRecompute, func(t *testing.T) error { return hzStoreRecompute(t) })
	check(t, horizonStoreLoss, func(t *testing.T) error { return hzStoreLoss(t) })
	check(t, horizonStorePaths, func(t *testing.T) error { return hzStorePaths(t) })
	check(t, horizonHoldings, func(t *testing.T) error { return hzHoldings(t) })
	check(t, horizonAboveTree, func(t *testing.T) error { return hzAboveTree(t) })
	check(t, horizonDeltaBase, func(t *testing.T) error { return hzDeltaBase(t) })
	check(t, horizonDerived, func(t *testing.T) error { return hzDerived(t) })
}

// hzFiles is the set of file blobs the check-ins rids of r name, read
// through manifest.ListFiles: the full tree, not the horizon's walk.
func hzFiles(t *testing.T, r *repo.Repo, rids ...libfossil.FslID) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, rid := range rids {
		files, err := manifest.ListFiles(r, rid)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			out[f.UUID] = true
		}
	}
	return out
}

func hzMinus(all, keep map[string]bool) []string {
	out := maps.Clone(all)
	for uuid := range keep {
		delete(out, uuid)
	}
	return scSorted(out)
}

func hzUUID(t *testing.T, r *repo.Repo, rid libfossil.FslID) string {
	t.Helper()
	var uuid string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	return uuid
}

func hzStoreCone(t *testing.T, c string, rule cone.HorizonRule, depth cone.HorizonDepth, include ...string) *cone.Cone {
	t.Helper()
	h, err := cone.NewHorizon(c, rule, depth)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := cone.NewWithHorizon(include, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// hzSet makes r's store cone a content horizon at c.
func hzSet(t *testing.T, r *repo.Repo, c string, rule cone.HorizonRule, include ...string) {
	t.Helper()
	if err := content.SetStoreCone(r.DB(), hzStoreCone(t, c, rule, cone.DepthContent, include...)); err != nil {
		t.Fatal(err)
	}
}

// newHZStore clones origin and turns every file blob of the check-ins
// into a phantom, so the clone holds manifests alone and its classification
// decides each blob. It returns the blobs it turned.
func newHZStore(t *testing.T, origin *scRepo, checkins ...libfossil.FslID) (*scSparse, map[string]bool) {
	t.Helper()
	s := &scSparse{origin: origin, gimmes: map[string]bool{}}
	if _, err := origin.r.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	clone, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "horizon.fossil"),
		s.transport(origin.r), sync.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clone.Close() })
	s.clone = clone
	files := hzFiles(t, origin.r, checkins...)
	for uuid := range files {
		scPhantomize(t, clone, uuid)
	}
	clear(s.gimmes)
	return s, files
}

func (g *hzGraph) checkins() []libfossil.FslID {
	return []libfossil.FslID{g.c1, g.c2, g.c3, g.c4, g.m, g.feat, g.side1, g.side2}
}

// The content horizon stands alone, and the structural horizon builds on it.
func hzContentAlone(t *testing.T) error {
	g := newHZGraph(t)
	s, files := newHZStore(t, g.f, g.checkins()...)
	c := hzUUID(t, g.f.r, g.c3)
	want := hzMinus(files, hzFiles(t, g.f.r, g.c3, g.c4, g.m, g.side1, g.side2))
	hzSet(t, s.clone, c, cone.RuleDescendants)
	if err := scSameList("elided", scClassed(scAbsences(t, s.clone), content.Elided), want); err != nil {
		return err
	}
	// The structural horizon presupposes the content horizon: over the same
	// store it elides the same blobs, cutting manifests besides.
	if err := content.SetStoreCone(s.clone.DB(),
		hzStoreCone(t, c, cone.RuleDescendants, cone.DepthStructural)); err != nil {
		return err
	}
	return scSameList("elided", scClassed(scAbsences(t, s.clone), content.Elided), want)
}

// A horizon naming C by its hash classifies by the held set under each
// rule: what K's check-ins name is wanted, everything else elided. K is the
// origin's, computed over the dense graph; the descendants rule's is also
// spelled out.
func hzStoreRules(t *testing.T) error {
	g := newHZGraph(t)
	s, files := newHZStore(t, g.f, g.checkins()...)
	c := hzUUID(t, g.f.r, g.c3)
	if err := hzWant(g.f.r, g.c3, cone.RuleDescendants, g.c3, g.c4, g.m, g.side1, g.side2); err != nil {
		return err
	}
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		k, err := horizon.HeldSet(context.Background(), g.f.r.DB(), g.c3, rule)
		if err != nil {
			return err
		}
		hzSet(t, s.clone, c, rule)
		classes := scAbsences(t, s.clone)
		above := hzFiles(t, g.f.r, k...)
		if err := scSameList(rule.String()+" elided", scClassed(classes, content.Elided), hzMinus(files, above)); err != nil {
			return err
		}
		if err := scSameList(rule.String()+" transit", scClassed(classes, content.Transit), scSorted(above)); err != nil {
			return err
		}
	}
	return nil
}

// A store holding manifests alone recomputes the origin's classification,
// and recomputes it again when a check-in arrives: the new check-in's blob
// is fetched, and no elided blob is ever asked for.
func hzStoreRecompute(t *testing.T) error {
	g := newHZGraph(t)
	s, files := newHZStore(t, g.f, g.checkins()...)
	hzSet(t, s.clone, hzUUID(t, g.f.r, g.c3), cone.RuleDescendants)
	below := hzMinus(files, hzFiles(t, g.f.r, g.c3, g.c4, g.m, g.side1, g.side2))
	if err := scSameList("elided", scClassed(scAbsences(t, s.clone), content.Elided), below); err != nil {
		return err
	}
	g.tip = hzCommit(t, g.f, "tip", []libfossil.FslID{g.side2}, nil)
	if err := s.sync(t, s.clone, g.f.r, false, true); err != nil {
		return err
	}
	for _, uuid := range below {
		if s.gimmes[uuid] {
			return expectf("the pull asked for elided %s", uuid)
		}
	}
	for uuid := range hzFiles(t, g.f.r, g.tip) {
		if _, ok := content.AvailableByUUID(s.clone.DB(), uuid); !ok {
			return expectf("the new check-in's blob %s was not fetched", uuid)
		}
	}
	classes := scAbsences(t, s.clone)
	if err := scSameList("elided after the pull", scClassed(classes, content.Elided), below); err != nil {
		return err
	}
	return scSameList("transit after the pull", scClassed(classes, content.Transit), nil)
}

// The horizon is store-adjacent: setting it adds no artifact, and losing it
// makes every elided blob transit, which the next pull fetches.
func hzStoreLoss(t *testing.T) error {
	g := newHZGraph(t)
	s, files := newHZStore(t, g.f, g.checkins()...)
	before := scTableDump(t, s.clone)
	hzSet(t, s.clone, hzUUID(t, g.f.r, g.c3), cone.RuleDescendants)
	if after := scTableDump(t, s.clone); after != before {
		return expectf("setting the horizon changed the artifact rows")
	}
	below := hzMinus(files, hzFiles(t, g.f.r, g.c3, g.c4, g.m, g.side1, g.side2))
	if err := scSameList("elided", scClassed(scAbsences(t, s.clone), content.Elided), below); err != nil {
		return err
	}
	later := []byte{0, 9, 0, 0, 0, 40} // a later version's lead-in
	if _, err := s.clone.DB().Exec("UPDATE storecone SET cone=?", later); err != nil {
		return err
	}
	classes := scAbsences(t, s.clone)
	if err := scSameList("transit after the loss", scClassed(classes, content.Transit), scSorted(files)); err != nil {
		return err
	}
	if err := s.sync(t, s.clone, g.f.r, false, true); err != nil {
		return err
	}
	report, err := scVerify(s.clone)
	if err != nil {
		return err
	}
	if report.Phantoms != 0 || report.Elided != 0 {
		return expectf("after a pull the store lacks %d transit and %d elided artifacts; want it dense",
			report.Phantoms, report.Elided)
	}
	return nil
}

// With path members as well, a blob is kept only when it is both above the
// horizon and covered; a manifest below the horizon and outside the paths
// stays wanted.
func hzStorePaths(t *testing.T) error {
	g := newHZGraph(t)
	s, files := newHZStore(t, g.f, g.checkins()...)
	manifestUUID := hzUUID(t, g.f.r, g.c2)
	scPhantomize(t, s.clone, manifestUUID)
	hzSet(t, s.clone, hzUUID(t, g.f.r, g.c3), cone.RuleDescendants, "c4", "src")
	classes := scAbsences(t, s.clone)
	kept := hzFiles(t, g.f.r, g.c4)
	if err := scSameList("elided", scClassed(classes, content.Elided), hzMinus(files, kept)); err != nil {
		return err
	}
	want := maps.Clone(kept)
	want[manifestUUID] = true
	return scSameList("transit", scClassed(classes, content.Transit), scSorted(want))
}

// A horizon clone pulled from a dense origin holds every manifest of every
// age, every blob K names, and elided phantoms for the rest; it verifies
// healthy with the elision counted.
func hzHoldings(t *testing.T) error {
	g := newHZGraph(t)
	s, files := newHZStore(t, g.f, g.checkins()...)
	hzSet(t, s.clone, hzUUID(t, g.f.r, g.c3), cone.RuleDescendants)
	if err := s.sync(t, s.clone, g.f.r, false, true); err != nil {
		return err
	}
	for _, rid := range g.checkins() {
		if _, ok := content.AvailableByUUID(s.clone.DB(), hzUUID(t, g.f.r, rid)); !ok {
			return expectf("check-in %d is not held", rid)
		}
	}
	above := hzFiles(t, g.f.r, g.c3, g.c4, g.m, g.side1, g.side2)
	for uuid := range above {
		if _, ok := content.AvailableByUUID(s.clone.DB(), uuid); !ok {
			return expectf("blob %s above the horizon is not held", uuid)
		}
	}
	below := hzMinus(files, above)
	if err := scSameList("elided", scClassed(scAbsences(t, s.clone), content.Elided), below); err != nil {
		return err
	}
	report, err := scVerify(s.clone)
	if err != nil {
		return err
	}
	if report.Elided != len(below) || report.Phantoms != 0 {
		return expectf("verify: elided %d, transit %d; want %d, 0", report.Elided, report.Phantoms, len(below))
	}
	return nil
}

// Over sub-manifest trees, a blob that entered below C and never changed is
// above: C names it through a node though mlink records it below C. The
// pull fetches every blob above and none below, and verification counts the
// elision apart from a transit phantom.
func hzAboveTree(t *testing.T) error {
	origin := newSCRepo(t, true)
	files := maps.Clone(scFiles)
	files["src/a.c"] = "a, changed\n"
	c2 := origin.commit(t, files, origin.c1)
	s, all := newHZStore(t, origin, origin.c1, c2)
	hzSet(t, s.clone, hzUUID(t, origin.r, c2), cone.RuleDescendants)
	old := scUUIDOf(t, origin.r, origin.c1, "src/a.c")
	if err := scSameList("elided", scClassed(scAbsences(t, s.clone), content.Elided), []string{old}); err != nil {
		return err
	}
	if err := s.sync(t, s.clone, origin.r, false, true); err != nil {
		return err
	}
	if s.gimmes[old] {
		return expectf("the pull asked for the elided %s", old)
	}
	for uuid := range all {
		if _, ok := content.AvailableByUUID(s.clone.DB(), uuid); ok == (uuid == old) {
			return expectf("after the pull %s held = %v", uuid, ok)
		}
	}
	report, err := scVerify(s.clone)
	if err != nil {
		return err
	}
	if report.Elided != 1 || report.Phantoms != 0 {
		return expectf("verify: elided %d, transit %d; want 1, 0", report.Elided, report.Phantoms)
	}
	scPhantomize(t, s.clone, scUUIDOf(t, origin.r, c2, "README"))
	if report, err = scVerify(s.clone); err != nil {
		return err
	}
	if report.Elided != 1 || report.Phantoms != 1 {
		return expectf("verify with a transit phantom: elided %d, transit %d; want 1, 1", report.Elided, report.Phantoms)
	}
	return nil
}

// A phantom a held delta names as its base is transit, below the horizon or
// not.
func hzDeltaBase(t *testing.T) error {
	g := newHZGraph(t)
	s, _ := newHZStore(t, g.f, g.checkins()...)
	hzSet(t, s.clone, hzUUID(t, g.f.r, g.c3), cone.RuleDescendants)
	base := scUUIDOf(t, g.f.r, g.c2, "c2")
	if got := scAbsences(t, s.clone)[base]; got != content.Elided {
		return expectf("c2's blob is %v before a delta names it, want elided", got)
	}
	target := []byte("derived from c2\n")
	d, err := delta.Create([]byte("c2\n"), target)
	if err != nil {
		return err
	}
	var brid libfossil.FslID
	if err := s.clone.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", base).Scan(&brid); err != nil {
		return err
	}
	if _, err := blob.StoreDeltaRaw(s.clone.DB(), hash.SHA1(target), d, brid, nil); err != nil {
		return err
	}
	if got := scAbsences(t, s.clone)[base]; got != content.Transit {
		return expectf("a below-horizon delta base is %v, want transit", got)
	}
	return nil
}

// Derived state is the dense derivation whatever the horizon: setting one
// and moving C to an ancestor (a deepen) reclassify the phantoms and touch
// no derived table, no artifact row and no phantom row. This
// implementation keeps no prefix-anchored checkpoint.
func hzDerived(t *testing.T) error {
	g := newHZGraph(t)
	s, files := newHZStore(t, g.f, g.checkins()...)
	dump := func() string {
		var out string
		if err := s.clone.DB().QueryRow(`SELECT
			(SELECT group_concat(x, ';') FROM (SELECT mid||':'||fid||':'||pid||':'||fnid AS x FROM mlink ORDER BY 1)) || '|' ||
			(SELECT group_concat(x, ';') FROM (SELECT pid||':'||cid||':'||isprim AS x FROM plink ORDER BY 1)) || '|' ||
			(SELECT group_concat(rid, ';') FROM (SELECT rid FROM leaf ORDER BY 1)) || '|' ||
			(SELECT group_concat(x, ';') FROM (SELECT tagid||':'||rid||':'||tagtype AS x FROM tagxref ORDER BY 1)) || '|' ||
			(SELECT group_concat(x, ';') FROM (SELECT objid||':'||mtime AS x FROM event ORDER BY 1)) || '|' ||
			(SELECT group_concat(name, ';') FROM (SELECT name FROM sqlite_master WHERE name<>'storecone' ORDER BY 1))`,
		).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out + scTableDump(t, s.clone)
	}
	before := dump()
	hzSet(t, s.clone, hzUUID(t, g.f.r, g.c3), cone.RuleDescendants)
	atC := scClassed(scAbsences(t, s.clone), content.Elided)
	hzSet(t, s.clone, hzUUID(t, g.f.r, g.c2), cone.RuleDescendants)
	atParent := scClassed(scAbsences(t, s.clone), content.Elided)
	want := hzMinus(files, hzFiles(t, g.f.r, g.c2, g.c3, g.c4, g.m, g.side1, g.side2))
	if err := scSameList("elided after the deepen", atParent, want); err != nil {
		return err
	}
	if len(atParent) >= len(atC) {
		return expectf("deepening elided %d, %d before", len(atParent), len(atC))
	}
	if after := dump(); after != before {
		return expectf("the horizon changed derived state:\n%s\nwas\n%s", after, before)
	}
	return nil
}
