package conformance

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/sync"
)

// A delta manifest in K whose baseline lies below C (draft-fossil-horizon-00
// §3.1, §5.1): the baseline is held without being in K, and the structural
// store derives the same branch and leaf state for K as the dense origin and
// shows the baseline as no leaf.
var horizonDeltaBaseline = Case{ID: "horizon.3.1.delta-baseline-below", Req: "HZ-3.1-4"}

func horizonDeltaCases() []Case { return []Case{horizonDeltaBaseline} }

func TestHorizonDeltaBaseline(t *testing.T) {
	check(t, horizonDeltaBaseline, func() error { return hzDeltaBaseline(t) })
}

func hzDeltaBaseline(t *testing.T) error {
	f := newSCSyncRepo(t, scSyncFiles())
	commit := func(parent libfossil.FslID, x string, delta bool) libfossil.FslID {
		t.Helper()
		files := []manifest.File{{Name: "x", Content: []byte(x)}}
		for name, body := range scSyncFiles() {
			files = append(files, manifest.File{Name: name, Content: []byte(body)})
		}
		f.when = f.when.Add(time.Hour)
		rid, _, err := manifest.Checkin(f.r, manifest.CheckinOpts{
			Files: files, Comment: x, User: "u", Parent: parent, Delta: delta, Time: f.when,
		})
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	c2 := commit(f.c1, "v2\n", false)
	c3 := commit(c2, "v3\n", true)
	c4 := commit(c3, "v4\n", true)
	for _, rid := range []libfossil.FslID{c3, c4} {
		d := hzDeckOf(t, f.r, rid)
		if d.B != hzUUID(t, f.r, c2) {
			return expectf("fixture: check-in %d B card %q, want the baseline %s", rid, d.B, hzUUID(t, f.r, c2))
		}
	}
	u3, u2 := hzUUID(t, f.r, c3), hzUUID(t, f.r, c2)
	h, err := cone.NewHorizon(u3, cone.RuleDescendants, cone.DepthStructural)
	if err != nil {
		return err
	}
	sc, err := cone.NewWithHorizon(nil, nil, h)
	if err != nil {
		return err
	}
	r, result, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "delta.fossil"),
		&scSyncPeer{r: f.r}, sync.CloneOpts{StoreCone: sc, PullPhantoms: true})
	if err != nil {
		return expectf("structural clone with a baseline below C: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	if result.HorizonFallback || result.StructuralServed == 0 {
		return expectf("clone fell back %v, served structural in %d rounds", result.HorizonFallback, result.StructuralServed)
	}
	if err := hzHeld(r, map[string]bool{u2: true}, true); err != nil {
		return expectf("baseline below C: %v", err)
	}
	for _, rid := range []libfossil.FslID{c3, c4} {
		u := hzUUID(t, f.r, rid)
		want, err := hzTagState(f.r.DB(), u)
		if err != nil {
			return err
		}
		got, err := hzTagState(r.DB(), u)
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			return expectf("check-in %.10s carries %v, dense %v", u, got, want)
		}
		var leafGot, leafWant int
		r.DB().QueryRow("SELECT count(*) FROM leaf JOIN blob USING(rid) WHERE uuid=?", u).Scan(&leafGot)
		f.r.DB().QueryRow("SELECT count(*) FROM leaf JOIN blob USING(rid) WHERE uuid=?", u).Scan(&leafWant)
		if leafGot != leafWant {
			return expectf("check-in %.10s leaf %d, dense %d", u, leafGot, leafWant)
		}
	}
	var leaf int
	r.DB().QueryRow("SELECT count(*) FROM leaf JOIN blob USING(rid) WHERE uuid=?", u2).Scan(&leaf)
	if leaf != 0 {
		return expectf("the baseline %.10s below C is shown as a leaf", u2)
	}
	return nil
}
