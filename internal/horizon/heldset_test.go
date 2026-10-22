package horizon_test

import (
	"context"
	"errors"
	"github.com/danmestas/go-libfossil/internal/cone"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

// dag builds check-ins in a fresh repository, one day apart, so the plink,
// tagxref and leaf rows are the ones a real commit writes.
type dag struct {
	t   *testing.T
	r   *repo.Repo
	day int
}

func newDAG(t *testing.T) *dag {
	t.Helper()
	r, err := repo.CreateWithEnv(t.TempDir()+"/h.fossil", "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return &dag{t: t, r: r}
}

// branchTags start the named branch on the check-in that carries them.
func branchTags(name string) []deck.TagCard {
	return []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: name},
		{Type: deck.TagPropagating, Name: "sym-" + name, UUID: "*"},
		{Type: deck.TagCancel, Name: "sym-trunk", UUID: "*"},
	}
}

var closedTag = deck.TagCard{Type: deck.TagSingleton, Name: "closed", UUID: "*"}

// commit checks in name with parents[0] as its primary parent and the rest
// as merge parents; no parents makes a root.
func (d *dag) commit(name string, parents []libfossil.FslID, tags ...deck.TagCard) libfossil.FslID {
	d.t.Helper()
	d.day++
	opts := manifest.CheckinOpts{
		Files:   []manifest.File{{Name: name + ".txt", Content: []byte(name + "\n")}},
		Comment: name,
		User:    "test",
		Time:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, d.day),
		Tags:    tags,
	}
	if len(parents) > 0 {
		opts.Parent = parents[0]
		opts.MergeParents = parents[1:]
	}
	rid, _, err := manifest.Checkin(d.r, opts)
	if err != nil {
		d.t.Fatalf("Checkin %s: %v", name, err)
	}
	return rid
}

func (d *dag) chain(names ...string) []libfossil.FslID {
	d.t.Helper()
	var rids []libfossil.FslID
	var parent []libfossil.FslID
	for _, name := range names {
		rid := d.commit(name, parent)
		rids = append(rids, rid)
		parent = []libfossil.FslID{rid}
	}
	return rids
}

func p(rids ...libfossil.FslID) []libfossil.FslID { return rids }

func (d *dag) want(c libfossil.FslID, rule cone.HorizonRule, want ...libfossil.FslID) {
	d.t.Helper()
	got, err := horizon.HeldSet(context.Background(), d.r.DB(), c, rule)
	if err != nil {
		d.t.Fatalf("HeldSet(%d, %v): %v", c, rule, err)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		d.t.Errorf("HeldSet(%d, %v) = %v, want %v", c, rule, got, want)
	}
}

func TestLinearMidChain(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3", "c4", "c5")
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		d.want(c[2], rule, c[2], c[3], c[4])
	}
}

func TestTipHoldsOnlyItself(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3")
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		d.want(c[2], rule, c[2])
	}
}

// A branch forked below C is outside the descendants set; the leaves rule
// holds it back to its fork, c1, which is the nearest common ancestor of
// its leaf and C. c2, between the fork and C, is on no such path.
func TestBranchForkedBelowC(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3", "c4")
	f1 := d.commit("f1", p(c[0]), branchTags("feat")...)
	f2 := d.commit("f2", p(f1))
	d.want(c[2], cone.RuleDescendants, c[2], c[3])
	d.want(c[2], cone.RuleLeaves, c[2], c[3], f1, f2, c[0])
}

func TestBranchForkedAtC(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3", "c4")
	f1 := d.commit("f1", p(c[2]), branchTags("feat")...)
	f2 := d.commit("f2", p(f1))
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		d.want(c[2], rule, c[2], c[3], f1, f2)
	}
}

// A below-C branch merged into a descendant of C: the merge is a descendant,
// the branch is only its ancestor, so the descendants rule leaves the branch
// out. fossil's leaf rule keeps f1 an open leaf (its one child is on trunk),
// so the leaves rule holds it back to the fork.
func TestBelowCBranchMergedIntoDescendant(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3", "c4")
	f1 := d.commit("f1", p(c[0]), branchTags("feat")...)
	m := d.commit("m", p(c[3], f1))
	d.want(c[2], cone.RuleDescendants, c[2], c[3], m)
	d.want(c[2], cone.RuleLeaves, c[2], c[3], m, f1, c[0])
}

// A below-C branch that merges a descendant of C in: following merge
// parents forward makes f2 a descendant, and its open leaf being in K adds
// nothing under the leaves rule. f1 is on no path from an open leaf back to
// that leaf's nearest common ancestor with C, which is C itself.
func TestDescendantMergedIntoBelowCBranch(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3", "c4")
	f1 := d.commit("f1", p(c[0]), branchTags("feat")...)
	f2 := d.commit("f2", p(f1, c[3]))
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		d.want(c[2], rule, c[2], c[3], f2)
	}
}

// C is a merge of x and y, and the open leaf l reaches both through its own
// two parents: both are nearest common ancestors of l and C, and both are
// held. r, below them, is not.
func TestSeveralNearestCommonAncestors(t *testing.T) {
	d := newDAG(t)
	r := d.commit("r", nil)
	x := d.commit("x", p(r))
	y := d.commit("y", p(r), branchTags("feat")...)
	cc := d.commit("c", p(x, y))
	x2 := d.commit("x2", p(x), branchTags("side")...)
	y2 := d.commit("y2", p(y), branchTags("side")...)
	l := d.commit("l", p(x2, y2))
	d.want(cc, cone.RuleDescendants, cc)
	d.want(cc, cone.RuleLeaves, cc, l, x2, y2, x, y)
}

func TestClosedLeafExcluded(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3", "c4")
	f1 := d.commit("f1", p(c[0]), branchTags("feat")...)
	d.commit("f2", p(f1), closedTag)
	g1 := d.commit("g1", p(c[1]), branchTags("gone")...)
	d.want(c[2], cone.RuleLeaves, c[2], c[3], g1, c[1])
}

// A leaf whose ancestry never meets C's has no common ancestor with it; the
// walk, like zit's, holds its whole ancestry rather than nothing.
func TestUnrelatedRootHeldWhole(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2")
	u1 := d.commit("u1", nil, branchTags("other")...)
	u2 := d.commit("u2", p(u1))
	d.want(c[1], cone.RuleDescendants, c[1])
	d.want(c[1], cone.RuleLeaves, c[1], u1, u2)
}

// A phantom parent is not a held check-in: no walk enters it.
func TestPhantomParentNeverHeld(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3")
	f1 := d.commit("f1", p(c[0]), branchTags("feat")...)
	ph, err := blob.StorePhantom(d.r.DB(), "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	for _, cid := range []libfossil.FslID{c[1], f1} {
		if _, err := d.r.DB().Exec(
			"INSERT INTO plink(pid, cid, isprim, mtime) VALUES(?, ?, 0, 2460000.5)", ph, cid); err != nil {
			t.Fatal(err)
		}
	}
	d.want(c[1], cone.RuleDescendants, c[1], c[2])
	d.want(c[1], cone.RuleLeaves, c[1], c[2], f1, c[0])
}

func TestCNotHeld(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2")
	ph, err := blob.StorePhantom(d.r.DB(), "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	var file libfossil.FslID
	if err := d.r.DB().QueryRow(
		"SELECT fid FROM mlink WHERE mid=?", c[0]).Scan(&file); err != nil {
		t.Fatal(err)
	}
	for _, rid := range []libfossil.FslID{c[1] + 1000, ph, file} {
		for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
			_, err := horizon.HeldSet(context.Background(), d.r.DB(), rid, rule)
			if !errors.Is(err, horizon.ErrNotHeld) {
				t.Errorf("HeldSet(%d, %v) error = %v, want ErrNotHeld", rid, rule, err)
			}
		}
	}
}

func TestBoundExceeded(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3", "c4", "c5")
	f1 := d.commit("f1", p(c[0]), branchTags("feat")...)
	d.commit("f2", p(f1))
	cases := []struct {
		rule  cone.HorizonRule
		c     libfossil.FslID
		limit int
	}{
		{cone.RuleDescendants, c[0], 3},  // the graph's 6 edges overflow the load
		{cone.RuleDescendants, c[0], 6},  // c1's 7 descendants overflow the walk
		{cone.RuleLeaves, c[3], 6},       // c4..c1 plus f1..f2 overflow it
		{cone.RuleLeaves, c[4], 1 << 20}, // a sanity row: within bounds
	}
	for _, tc := range cases {
		_, err := horizon.HeldSetWithin(context.Background(), d.r.DB(), tc.c, tc.rule, tc.limit)
		if tc.limit == 1<<20 {
			if err != nil {
				t.Errorf("rule %v limit %d: %v", tc.rule, tc.limit, err)
			}
			continue
		}
		if !errors.Is(err, horizon.ErrBound) {
			t.Errorf("rule %v C %d limit %d: error = %v, want ErrBound", tc.rule, tc.c, tc.limit, err)
		}
	}
}

func TestCancelled(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := horizon.HeldSet(ctx, d.r.DB(), c[0], cone.RuleLeaves); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestDeterministic(t *testing.T) {
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3", "c4")
	f1 := d.commit("f1", p(c[0]), branchTags("feat")...)
	d.commit("m", p(c[3], f1))
	g1 := d.commit("g1", p(c[1]), branchTags("g")...)
	d.commit("g2", p(g1, c[2]))
	for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
		first, err := horizon.HeldSet(context.Background(), d.r.DB(), c[2], rule)
		if err != nil {
			t.Fatal(err)
		}
		for range 5 {
			again, err := horizon.HeldSet(context.Background(), d.r.DB(), c[2], rule)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(first, again) {
				t.Fatalf("rule %v: %v then %v", rule, first, again)
			}
		}
		if !slices.IsSorted(first) {
			t.Fatalf("rule %v: %v is not sorted", rule, first)
		}
	}
}
