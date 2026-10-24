package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/xfer"
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

// TestHorizonSeedDateZit is TestHorizonSeedZit over a repository whose
// newest tag event is a cancel: both implementations date the seed at the
// repository's latest tag event (§5.2.2), so the seeds are the same bytes.
// A zit before check-in caeef421ee dates it at the latest tag in force,
// earlier, and no string in its binary tells it apart, so the test detects
// that from the seed and skips unless REQUIRE_ZIT_SEED_DATE=1; CI's pinned
// zit 0.7.1 predates it.
func TestHorizonSeedDateZit(t *testing.T) {
	bin := zitHorizonBinary(t)
	s := newHZSeed(t)
	s.tag(t, s.c3, "late", tag.TagSingleton, "")
	s.tag(t, s.c3, "late", tag.TagCancel, "")
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
	structural := filepath.Join(dir, "structural")
	zitRun(t, bin, home, dir, "clone", url, structural, "--no-open", "--once",
		"--horizon="+hzUUID(t, s.f.r, s.c3), "--structural")
	zitSeed, err := os.ReadFile(filepath.Join(structural, ".zit-cache", "horizon-seed"))
	if err != nil {
		t.Fatalf("zit held no seed: %v", err)
	}
	goName, goSeed, err := s.mint("zit-seed")
	if err != nil {
		t.Fatal(err)
	}
	if string(goSeed) == string(zitSeed) && goName == hash.SHA3(zitSeed) {
		return
	}
	zd, zerr := deck.Parse(zitSeed)
	gd, gerr := deck.Parse(goSeed)
	if zerr == nil && gerr == nil && zd.D.Before(gd.D) && os.Getenv("REQUIRE_ZIT_SEED_DATE") != "1" {
		t.Skipf("%s (%s) dates the seed at the latest tag in force (%v, go-libfossil %v): the run needs zit "+
			"at check-in caeef421ee or later", bin, zitVersion(bin), zd.D, gd.D)
	}
	t.Errorf("seeds differ:\nzit %s\n%s\ngo  %s\n%s", hash.SHA3(zitSeed), zitSeed, goName, goSeed)
}
