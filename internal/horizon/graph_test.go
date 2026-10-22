package horizon_test

import (
	"context"
	"slices"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
)

// draft-fossil-horizon-00 §6.2: a graph loaded once and extended by the
// check-ins appended since yields, for every C and both rules, the held set
// a fresh read of plink does. The appended check-ins include a fork from
// below every C, which under the leaves rule brings old check-ins into K.
func TestGraphExtendMatchesHeldSet(t *testing.T) {
	ctx := context.Background()
	d := newDAG(t)
	c := d.chain("c1", "c2", "c3", "c4")
	g, err := horizon.LoadGraph(ctx, d.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	f1 := d.commit("f1", p(c[0]), branchTags("feat")...)
	f2 := d.commit("f2", p(f1))
	m := d.commit("m", p(c[3], f2))
	g1 := d.commit("g1", p(c[1]), branchTags("gone")...)
	g, err = g.Extend(ctx, d.r.DB(), []libfossil.FslID{f1, f2})
	if err != nil {
		t.Fatal(err)
	}
	g, err = g.Extend(ctx, d.r.DB(), []libfossil.FslID{m, g1})
	if err != nil {
		t.Fatal(err)
	}
	all := append(slices.Clone(c), f1, f2, m, g1)
	for _, at := range all {
		for _, rule := range []cone.HorizonRule{cone.RuleDescendants, cone.RuleLeaves} {
			want, err := horizon.HeldSet(ctx, d.r.DB(), at, rule)
			if err != nil {
				t.Fatal(err)
			}
			got, err := g.HeldSet(ctx, at, rule)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("C=%d %v: extended graph %v, fresh %v", at, rule, got, want)
			}
		}
	}
	if _, err := g.HeldSet(ctx, 1<<30, cone.RuleLeaves); err == nil {
		t.Fatal("a C the graph does not hold placed")
	}
}

// Extending leaves the graph it extends as it was: an origin's request that
// rolls back must not change the classification another request shares.
func TestGraphExtendCopies(t *testing.T) {
	ctx := context.Background()
	d := newDAG(t)
	c := d.chain("c1", "c2")
	g, err := horizon.LoadGraph(ctx, d.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	c3 := d.commit("c3", p(c[1]))
	if _, err := g.Extend(ctx, d.r.DB(), []libfossil.FslID{c3}); err != nil {
		t.Fatal(err)
	}
	got, err := g.HeldSet(ctx, c[0], cone.RuleDescendants)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, c) {
		t.Fatalf("extension changed the graph it extended: %v", got)
	}
}
