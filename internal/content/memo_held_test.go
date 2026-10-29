package content_test

import (
	"context"
	"slices"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/horizon"
)

// A memo's held set equals horizon.Place's, is kept across published views
// of an unchanged store, and is recomputed once the store gains a check-in
// that changes K.
func TestMemoHeldSetKeptPerStore(t *testing.T) {
	ctx := context.Background()
	m := newMemoRepo(t, hzFull)
	root := m.commit(0)
	c := m.commit(root)
	h, err := cone.NewHorizon(m.uuid(c), cone.RuleDescendants, cone.DepthContent)
	if err != nil {
		t.Fatal(err)
	}
	memo := content.NewMemo()
	held := func() []int64 {
		t.Helper()
		v, err := memo.View(ctx, m.r.DB())
		if err != nil {
			t.Fatal(err)
		}
		k, placed, err := v.HeldSet(ctx, m.r.DB(), h)
		if err != nil || !placed {
			t.Fatalf("HeldSet: placed %v, %v", placed, err)
		}
		want, _, err := horizon.Place(ctx, m.r.DB(), h)
		if err != nil || !slices.Equal(k, want) {
			t.Fatalf("HeldSet %v, horizon.Place %v (%v)", k, want, err)
		}
		v.Publish()
		out := make([]int64, len(k))
		for i, rid := range k {
			out[i] = int64(rid)
		}
		return out
	}
	first := held()
	v, err := memo.View(ctx, m.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ := v.HeldSet(ctx, m.r.DB(), h)
	v.Publish()
	again, err := memo.View(ctx, m.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := again.HeldSet(ctx, m.r.DB(), h)
	if len(a) == 0 || &a[0] != &b[0] {
		t.Fatal("an unchanged store's held set was recomputed by a later view")
	}
	child := m.commit(c)
	if after := held(); len(after) != len(first)+1 || !slices.Contains(after, int64(child)) {
		t.Fatalf("after a new child the held set is %v, want %v plus %d", after, first, child)
	}
	unknown, err := cone.NewHorizon("ab"+m.uuid(c)[2:], cone.RuleDescendants, cone.DepthContent)
	if err != nil {
		t.Fatal(err)
	}
	if _, placed, err := v.HeldSet(ctx, m.r.DB(), unknown); err != nil || placed {
		t.Fatalf("an unknown C: placed %v, %v", placed, err)
	}
}
