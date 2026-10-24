package conformance

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
)

// A seed's D is the latest tag event in the repository, cancels included
// (§5.2.2), so no change to tag state yields an earlier D and a clone's D
// floor (§5.2.4) never refuses a re-seed as a replay. These mirror zit's
// "horizon seed date" regression (src/horizon_e2e.zig).

func horizonSeedDateCases() []Case {
	return []Case{horizonSeedDateCancel, horizonSeedDateAncestor}
}

var (
	horizonSeedDateCancel   = Case{ID: "horizon.5.2.2.seed-date-cancel", Req: "HZ-5.2-2"}
	horizonSeedDateAncestor = Case{ID: "horizon.5.2.2.seed-date-ancestor-cancel", Req: "HZ-5.2-2"}
)

// hzDateStep is one tag event at the origin.
type hzDateStep struct {
	target func(x *hzStruct) libfossil.FslID
	name   string
	typ    int
}

func TestHorizonSeedDate(t *testing.T) {
	type step = hzDateStep
	onC := func(x *hzStruct) libfossil.FslID { return x.s.c3 }
	onAncestor := func(x *hzStruct) libfossil.FslID { return x.s.c1 }
	check(t, horizonSeedDateCancel, func() error {
		return hzSeedDateSteps(t, []step{
			{onC, "late", tag.TagSingleton},
			{onC, "late", tag.TagCancel},
			{onC, "closed", tag.TagSingleton},
			{onC, "closed", tag.TagCancel},
		})
	})
	// A tag propagated from c1, below C, becomes the newest row at C;
	// cancelling it at c1 deletes the row it propagated to C, the latest of
	// the rows the old D was dated from. The cancel's own row stays at c1.
	check(t, horizonSeedDateAncestor, func() error {
		return hzSeedDateSteps(t, []step{
			{onAncestor, "tint", tag.TagPropagating},
			{onAncestor, "tint", tag.TagCancel},
		})
	})
}

// hzSeedDateSteps clones the structural store, then applies each tag event
// at the origin, an hour after the last, pulling after each: the clone must
// take every re-seed and stay structural.
func hzSeedDateSteps(t *testing.T, steps []hzDateStep) error {
	x := newHZStruct(t)
	r, _, err := x.clone(t, x.peer())
	if err != nil {
		return err
	}
	for i, s := range steps {
		x.s.tag(t, s.target(x), s.name, s.typ, "")
		before, err := seed.Held(r.DB())
		if err != nil {
			return err
		}
		res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true})
		if err != nil {
			return expectf("step %d (%s %d): pull: %v", i, s.name, s.typ, err)
		}
		if len(res.Errors) != 0 || res.HorizonFallback {
			return expectf("step %d (%s %d): errors %q, fallback %v", i, s.name, s.typ, res.Errors, res.HorizonFallback)
		}
		if err := hzStillStructural(r); err != nil {
			return expectf("step %d (%s %d): %v", i, s.name, s.typ, err)
		}
		after, err := seed.Held(r.DB())
		if err != nil || after == nil || string(after) == string(before) {
			return expectf("step %d (%s %d): the clone did not take the re-seed (%v)", i, s.name, s.typ, err)
		}
	}
	return nil
}

func hzStillStructural(r *repo.Repo) error {
	d, err := hzStructDepth(r)
	if err != nil {
		return err
	}
	if d != cone.DepthStructural {
		return expectf("the store fell back to depth %v", d)
	}
	return nil
}
