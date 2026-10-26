package conformance

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// A delta manifest in K whose baseline lies below C (draft-fossil-horizon-00
// §3.1, §5.1): the baseline is held without being in K, and the structural
// store derives the same branch and leaf state for K as the dense origin and
// shows the baseline as no leaf.
var horizonDeltaBaseline = Case{ID: "horizon.3.1.delta-baseline-below", Req: "HZ-3.1-4"}

// The parents of that held baseline are boundary hashes as much as those of
// K's check-ins (§5.3, §6.2.2): the origin labels c1, the baseline c2's
// parent, in the reply that delivers c2, and the clone holds c1 as an elided
// phantom, never asking for it.
var horizonDeltaBoundary = Case{ID: "horizon.5.3.baseline-parent", Req: "HZ-5.3-2"}

func horizonDeltaCases() []Case { return []Case{horizonDeltaBaseline, horizonDeltaBoundary} }

func TestHorizonDeltaBaseline(t *testing.T) {
	check(t, horizonDeltaBaseline, func(t *testing.T) error { return hzDeltaBaseline(t) })
	check(t, horizonDeltaBoundary, func(t *testing.T) error { return hzDeltaBoundary(t) })
}

// hzDelta is c1 -> c2 -> c3 -> c4 at an origin, c3 and c4 delta manifests
// on the baseline c2, and a structural clone of it at C = c3.
type hzDelta struct {
	f              *scRepo
	peer           *scSyncPeer
	r              *repo.Repo
	c1, c2, c3, c4 libfossil.FslID
}

func newHZDelta(t *testing.T) (*hzDelta, error) {
	t.Helper()
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
			return nil, expectf("fixture: check-in %d B card %q, want the baseline %s", rid, d.B, hzUUID(t, f.r, c2))
		}
	}
	h, err := cone.NewHorizon(hzUUID(t, f.r, c3), cone.RuleDescendants, cone.DepthStructural)
	if err != nil {
		return nil, err
	}
	sc, err := cone.NewWithHorizon(nil, nil, h)
	if err != nil {
		return nil, err
	}
	peer := &scSyncPeer{r: f.r}
	r, result, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "delta.fossil"),
		peer, sync.CloneOpts{StoreCone: sc, PullPhantoms: true})
	if err != nil {
		return nil, expectf("structural clone with a baseline below C: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	if result.HorizonFallback || result.StructuralServed == 0 {
		return nil, expectf("clone fell back %v, served structural in %d rounds", result.HorizonFallback, result.StructuralServed)
	}
	return &hzDelta{f: f, peer: peer, r: r, c1: f.c1, c2: c2, c3: c3, c4: c4}, nil
}

func hzDeltaBaseline(t *testing.T) error {
	x, err := newHZDelta(t)
	if err != nil {
		return err
	}
	f, r, c3, c4 := x.f, x.r, x.c3, x.c4
	u2 := hzUUID(t, f.r, x.c2)
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

func hzDeltaBoundary(t *testing.T) error {
	x, err := newHZDelta(t)
	if err != nil {
		return err
	}
	u1, u2 := hzUUID(t, x.f.r, x.c1), hzUUID(t, x.f.r, x.c2)
	labelled := -1
	for i, reply := range x.peer.replies {
		for _, v := range hzPragmas(reply, "horizon-below") {
			if len(v) == 1 && v[0] == u1 && labelled < 0 {
				labelled = i
			}
		}
	}
	delivered := -1
	for i, reply := range x.peer.replies {
		for _, c := range reply.Cards {
			var uuid string
			switch fc := c.(type) {
			case *xfer.FileCard:
				uuid = fc.UUID
			case *xfer.CFileCard:
				uuid = fc.UUID
			}
			if uuid == u2 && delivered < 0 {
				delivered = i
			}
		}
	}
	if labelled < 0 || delivered < 0 || labelled > delivered {
		return expectf("c1 labelled in reply %d, the baseline c2 delivered in reply %d; want the label no later", labelled, delivered)
	}
	for _, req := range x.peer.requests {
		for _, c := range req.Cards {
			if g, ok := c.(*xfer.GimmeCard); ok && g.UUID == u1 {
				return expectf("the clone asked for c1, the baseline's labelled parent")
			}
		}
	}
	a, err := content.LoadAbsences(x.r.DB())
	if err != nil {
		return err
	}
	rid, err := hzRID(x.r, u1)
	if err != nil {
		return expectf("the clone holds no phantom for c1: %v", err)
	}
	if got := a.Class(rid); got != content.Elided {
		return expectf("c1 is held as %v, want elided", got)
	}
	return nil
}
