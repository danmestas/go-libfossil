package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/shun"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// A seed's D is the latest tag event in the repository, cancels included
// (§5.2.2), so no change to tag state yields an earlier D and a clone's D
// floor (§5.2.4) never refuses a re-seed as a replay. These mirror zit's
// "horizon seed date" regression (src/horizon_e2e.zig).

func horizonSeedDateCases() []Case {
	return []Case{horizonSeedDateCancel, horizonSeedDateAncestor, horizonSeedDateShun}
}

var (
	horizonSeedDateCancel   = Case{ID: "horizon.5.2.2.seed-date-cancel", Req: "HZ-5.2-2"}
	horizonSeedDateAncestor = Case{ID: "horizon.5.2.2.seed-date-ancestor-cancel", Req: "HZ-5.2-2"}
	horizonSeedDateShun     = Case{ID: "horizon.5.2.2.seed-date-shun", Req: "HZ-5.2-2"}
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
	check(t, horizonSeedDateCancel, func(t *testing.T) error {
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
	check(t, horizonSeedDateAncestor, func(t *testing.T) error {
		return hzSeedDateSteps(t, []step{
			{onAncestor, "tint", tag.TagPropagating},
			{onAncestor, "tint", tag.TagCancel},
		})
	})
}

// TestHorizonSeedDateShun pushes the origin's latest tag event far into the
// future, pulls, then shuns and purges that tag and rebuilds, which removes
// its rows, and tags again at an ordinary time. Dated from the rows left,
// the next seed would be earlier than the clone's and refused as a replay;
// the origin's minted floor (§5.2.2) dates it no earlier. This mirrors
// zit's shun regression at check-in 39546186f4 (src/horizon_e2e.zig).
func TestHorizonSeedDateShun(t *testing.T) {
	check(t, horizonSeedDateShun, func(t *testing.T) error {
		x := newHZStruct(t)
		r, _, err := x.clone(t, x.peer())
		if err != nil {
			return err
		}
		future := time.Unix(2500000000, 0).UTC()
		rid, err := tag.AddTag(x.s.f.r, tag.TagOpts{
			TargetRID: x.s.c3, TagName: "future", TagType: tag.TagSingleton, User: "u", Time: future,
		})
		if err != nil {
			return err
		}
		if err := hzSeedDatePull(r, x, "future tag"); err != nil {
			return err
		}
		held, err := seed.Held(r.DB())
		if err != nil {
			return err
		}
		if err := hzShunAndPurge(x.s.f.r, rid); err != nil {
			return err
		}
		x.s.tag(t, x.s.c3, "later", tag.TagSingleton, "")
		if err := hzSeedDatePull(r, x, "after the shun"); err != nil {
			return err
		}
		after, err := seed.Held(r.DB())
		if err != nil || after == nil || string(after) == string(held) {
			return expectf("after the shun: the clone did not take the re-seed (%v)", err)
		}
		d, err := deck.Parse(after)
		if err != nil {
			return err
		}
		if d.D.Before(future) {
			return expectf("after the shun: the seed is dated %v, before the floor %v", d.D, future)
		}
		return nil
	})
}

// hzSeedDatePull pulls r from x's origin: it must stay structural.
func hzSeedDatePull(r *repo.Repo, x *hzStruct, step string) error {
	res, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true})
	if err != nil {
		return expectf("%s: pull: %v", step, err)
	}
	if len(res.Errors) != 0 || res.HorizonFallback {
		return expectf("%s: errors %q, fallback %v", step, res.Errors, res.HorizonFallback)
	}
	if err := hzStillStructural(r); err != nil {
		return expectf("%s: %v", step, err)
	}
	return nil
}

// hzShunAndPurge shuns the artifact at rid, purges it and rebuilds, as an
// operator removes a bad tag: its tagxref rows go with it.
func hzShunAndPurge(r *repo.Repo, rid libfossil.FslID) error {
	var uuid string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		return err
	}
	if err := shun.Add(r.DB(), uuid, "far-future tag"); err != nil {
		return err
	}
	if _, err := shun.Purge(r.DB()); err != nil {
		return err
	}
	if _, err := verify.Rebuild(r); err != nil {
		return err
	}
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM tagxref WHERE srcid=?", rid).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return expectf("the purged tag left %d tagxref rows", n)
	}
	return nil
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
// zit 0.7.3 dates seeds this way, and CI sets REQUIRE_ZIT_SEED_DATE=1.
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
