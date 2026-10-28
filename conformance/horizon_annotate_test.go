package conformance

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/annotate"
	"github.com/danmestas/go-libfossil/internal/cone"
	icontent "github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
)

var horizonStructAnnotate = Case{ID: "horizon.5.5.annotate-crossing", Req: "HZ-5.5-1"}

func TestHorizonAnnotateCrossing(t *testing.T) {
	check(t, horizonStructAnnotate, func(t *testing.T) error { return hzAnnotateCrossing(t) })
}

// hzAnnotateCrossing: annotating back past C (draft-fossil-horizon-00 §5.5).
// The origin's f gains a line at each of c1, c2 = C and c3, and g is
// unchanged since c1. In a clone at C's structural horizon the lines whose
// origin lies at or below C -- f's first two, g's -- are reported as
// originating at the horizon, and f's c3 line keeps c3. A clone at C's
// content horizon holds every manifest (§4.1): it marks no line, and g, whose
// blob K names, is attributed to c1 as the origin attributes it.
func hzAnnotateCrossing(t *testing.T) error {
	origin := newSCSyncRepo(t, map[string]string{"f": "old\n", "g": "shared\n"})
	c2 := origin.commit(t, map[string]string{"f": "old\nmid\n", "g": "shared\n"}, origin.c1)
	c3 := origin.commit(t, map[string]string{"f": "old\nmid\nnew\n", "g": "shared\n"}, c2)
	c, tip, below := hzUUID(t, origin.r, c2), hzUUID(t, origin.r, c3), hzUUID(t, origin.r, origin.c1)
	clone := func(depth cone.HorizonDepth) (*repo.Repo, error) {
		h, err := cone.NewHorizon(c, cone.RuleDescendants, depth)
		if err != nil {
			return nil, err
		}
		sc, err := cone.NewWithHorizon(nil, nil, h)
		if err != nil {
			return nil, err
		}
		r, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"),
			&scSyncPeer{r: origin.r}, sync.CloneOpts{StoreCone: sc, PullPhantoms: true})
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { r.Close() })
		return r, nil
	}
	type want struct {
		uuid  string
		below bool
	}
	run := func(r *repo.Repo, file string, wants []want) error {
		rid, err := hzRID(r, tip)
		if err != nil {
			return err
		}
		lines, err := annotate.Annotate(r, annotate.Options{FilePath: file, StartRID: rid})
		if err != nil {
			return err
		}
		if len(lines) != len(wants) {
			return expectf("%s: %d lines, want %d", file, len(lines), len(wants))
		}
		for i, w := range wants {
			if lines[i].Version.UUID != w.uuid || lines[i].BelowHorizon != w.below {
				return expectf("%s line %d %q: %.10s below=%v, want %.10s below=%v", file, i, lines[i].Text,
					lines[i].Version.UUID, lines[i].BelowHorizon, w.uuid, w.below)
			}
		}
		return nil
	}
	structural, err := clone(cone.DepthStructural)
	if err != nil {
		return err
	}
	if err := run(structural, "f", []want{{c, true}, {c, true}, {tip, false}}); err != nil {
		return err
	}
	if err := run(structural, "g", []want{{c, true}}); err != nil {
		return err
	}
	content, err := clone(cone.DepthContent)
	if err != nil {
		return err
	}
	if err := run(content, "g", []want{{below, false}}); err != nil {
		return err
	}
	// f's older versions are blobs below the horizon, which only a demand
	// fetch brings (§4.4); what annotate makes of their absence is no
	// crossing of a structural horizon: a refusal naming them for that
	// fetch, or lines none of which is marked below the horizon.
	rid, err := hzRID(content, tip)
	if err != nil {
		return err
	}
	lines, err := annotate.Annotate(content, annotate.Options{FilePath: "f", StartRID: rid})
	var missing *icontent.MissingBelowHorizon
	if errors.As(err, &missing) {
		if len(missing.Checkins) != 0 || len(missing.Names) != 1 {
			return expectf("a content horizon's refusal names %v and %v, want f's payload at c1",
				missing.Checkins, missing.Names)
		}
		return nil
	}
	if err != nil {
		return err
	}
	for _, l := range lines {
		if l.BelowHorizon {
			return expectf("a content horizon marked %q below the horizon", l.Text)
		}
	}
	return nil
}
