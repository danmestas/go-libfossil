package annotate

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// horizonStore copies the check-ins of dense into a store at depth's
// horizon at c: their manifests and the blobs they name, crosslinked, with
// c's parents labelled below it (draft-fossil-horizon-00 §5.3). A content
// horizon also holds every older manifest, so it is copied whole.
func horizonStore(
	t *testing.T, dense *repo.Repo, c libfossil.FslID, keep []libfossil.FslID, depth cone.HorizonDepth,
) *repo.Repo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "horizon.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	var rids []libfossil.FslID
	var below []string
	for _, rid := range keep {
		data, err := content.Expand(dense.DB(), rid)
		if err != nil {
			t.Fatal(err)
		}
		d, err := deck.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		rids = append(rids, rid)
		for _, f := range d.F {
			frid, ok := content.AvailableByUUID(dense.DB(), f.UUID)
			if !ok {
				t.Fatalf("fixture: %s is not held", f.Name)
			}
			rids = append(rids, frid)
		}
		if rid == c {
			below = d.P
		}
	}
	err = r.WithTx(func(tx *db.Tx) error {
		for _, rid := range rids {
			data, err := content.Expand(dense.DB(), rid)
			if err != nil {
				return err
			}
			if _, _, err := blob.Store(tx, data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Crosslink(r); err != nil {
		t.Fatal(err)
	}
	h, err := cone.NewHorizon(uuidOf(t, dense, c), cone.RuleDescendants, depth)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := cone.NewWithHorizon(nil, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), sc); err != nil {
		t.Fatal(err)
	}
	if err := content.LabelBelow(r.DB(), below); err != nil {
		t.Fatal(err)
	}
	return r
}

func uuidOf(t *testing.T, r *repo.Repo, rid libfossil.FslID) string {
	t.Helper()
	var uuid string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	return uuid
}

func ridOf(t *testing.T, r *repo.Repo, uuid string) libfossil.FslID {
	t.Helper()
	var rid libfossil.FslID
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	return rid
}

// draft-fossil-horizon-00 §5.5: in a store at a structural horizon at C, a
// line whose origin lies at or below C -- line A from commit 1, line C from
// C itself, whose parent the store does not hold -- is reported as
// originating at the horizon, never as the work of the oldest version the
// walk reached; a line from above C keeps its version.
func TestAnnotateStructuralHorizon(t *testing.T) {
	dense, _, rid2, rid3 := setupThreeCommits(t)
	c, tip := uuidOf(t, dense, rid2), uuidOf(t, dense, rid3)
	r := horizonStore(t, dense, rid2, []libfossil.FslID{rid2, rid3}, cone.DepthStructural)
	lines, err := Annotate(r, Options{FilePath: "file.txt", StartRID: ridOf(t, r, tip)})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		text, uuid string
		below      bool
	}{{"line A", c, true}, {"line C", c, true}, {"line D", tip, false}}
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d", len(lines), len(want))
	}
	for i, w := range want {
		l := lines[i]
		if l.Text != w.text || l.Version.UUID != w.uuid || l.BelowHorizon != w.below {
			t.Errorf("line %d = %q %.10s below=%v, want %q %.10s below=%v",
				i, l.Text, l.Version.UUID, l.BelowHorizon, w.text, w.uuid, w.below)
		}
	}
}

// A content horizon holds every manifest (draft-fossil-horizon-00 §4.1), so
// annotate there walks the whole history as it does densely and marks no
// line below the horizon.
func TestAnnotateContentHorizon(t *testing.T) {
	dense, rid1, rid2, rid3 := setupThreeCommits(t)
	r := horizonStore(t, dense, rid2, []libfossil.FslID{rid1, rid2, rid3}, cone.DepthContent)
	want, err := Annotate(dense, Options{FilePath: "file.txt", StartRID: rid3})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Annotate(r, Options{FilePath: "file.txt", StartRID: ridOf(t, r, uuidOf(t, dense, rid3))})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Text != want[i].Text || got[i].Version.UUID != want[i].Version.UUID || got[i].BelowHorizon {
			t.Errorf("line %d = %q %.10s below=%v, dense %q %.10s", i, got[i].Text, got[i].Version.UUID,
				got[i].BelowHorizon, want[i].Text, want[i].Version.UUID)
		}
	}
}
