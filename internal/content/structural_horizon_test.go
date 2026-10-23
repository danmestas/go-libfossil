package content_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/simio"
)

// structuralStore is what a structural-horizon clone at c3 of hzFixture
// holds (draft-fossil-horizon-00 §5.1): c3's manifest and the blobs it names,
// and a control artifact tagging c1, crosslinked. c2, c3's parent, is a
// phantom only c3's P card names; c1 is a phantom only the tag names.
func structuralStore(t *testing.T) (r *repo.Repo, c1, c2, c3 string) {
	t.Helper()
	f := newHZFixtureHeld(t)
	when := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	ctl, err := tag.AddTag(f.r, tag.TagOpts{
		TargetRID: f.c1, TagName: "hue", TagType: tag.TagPropagating, Value: "red", User: "u", Time: when,
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err = repo.Create(filepath.Join(t.TempDir(), "structural.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	rids := []libfossil.FslID{f.c3, ctl}
	for _, fc := range hzDeck(t, f.r, f.c3).F {
		rid, ok := content.AvailableByUUID(f.r.DB(), fc.UUID)
		if !ok {
			t.Fatalf("fixture: %s is not held", fc.Name)
		}
		rids = append(rids, rid)
	}
	err = r.WithTx(func(tx *db.Tx) error {
		for _, rid := range rids {
			data, err := content.Expand(f.r.DB(), rid)
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
	return r, f.uuidOf(t, f.c1), f.uuidOf(t, f.c2), f.uuidOf(t, f.c3)
}

func structuralCone(t *testing.T, c string, depth cone.HorizonDepth) *cone.Cone {
	t.Helper()
	h, err := cone.NewHorizon(c, cone.RuleDescendants, depth)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := cone.NewWithHorizon(nil, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func classOf(t *testing.T, r *repo.Repo, uuid string) content.Absence {
	t.Helper()
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	rid, ok := blobRID(r, uuid)
	if !ok {
		t.Fatalf("%s is not named by the store", uuid)
	}
	return a.Class(rid)
}

func blobRID(r *repo.Repo, uuid string) (libfossil.FslID, bool) {
	var rid libfossil.FslID
	err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid)
	return rid, err == nil
}

// A structural horizon is stored. Under it an unlabelled parent of a held
// check-in is a transit phantom and a labelled one elided (§5.3); a phantom
// only a control artifact names is a manifest below the horizon nothing in
// K reaches, elided (§5.1); and the labels belong to the horizon they were
// given under, so moving C drops them. A content horizon is unchanged.
func TestStructuralHorizonClassification(t *testing.T) {
	r, c1, c2, c3 := structuralStore(t)
	if err := content.SetStoreCone(r.DB(), structuralCone(t, c3, cone.DepthStructural)); err != nil {
		t.Fatalf("storing a structural horizon: %v", err)
	}
	if got := classOf(t, r, c2); got != content.Transit {
		t.Fatalf("unlabelled parent c2 is %v, want transit", got)
	}
	if got := classOf(t, r, c1); got != content.Elided {
		t.Fatalf("c1, named only by a tag, is %v, want elided", got)
	}
	if err := content.LabelBelow(r.DB(), []string{c2}); err != nil {
		t.Fatal(err)
	}
	if got := classOf(t, r, c2); got != content.Elided {
		t.Fatalf("labelled parent c2 is %v, want elided", got)
	}
	labels, err := content.BelowLabels(r.DB())
	if err != nil || len(labels) != 1 || !labels[c2] {
		t.Fatalf("labels %v, %v; want c2", labels, err)
	}

	if err := content.SetStoreCone(r.DB(), structuralCone(t, c2, cone.DepthStructural)); err != nil {
		t.Fatal(err)
	}
	if labels, err := content.BelowLabels(r.DB()); err != nil || len(labels) != 0 {
		t.Fatalf("moving C kept labels %v, %v", labels, err)
	}

	if err := content.SetStoreCone(r.DB(), structuralCone(t, c3, cone.DepthContent)); err != nil {
		t.Fatal(err)
	}
	if err := content.LabelBelow(r.DB(), []string{c2}); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{c1, c2} {
		if got := classOf(t, r, uuid); got != content.Transit {
			t.Fatalf("under a content horizon %s is %v, want transit", uuid, got)
		}
	}
}

// newHZFixtureHeld is hzFixture with every artifact still held.
func newHZFixtureHeld(t *testing.T) *hzFixture {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "hz.fossil"), "u", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	f := &hzFixture{r: r, blob: map[string]libfossil.FslID{}, uuid: map[string]string{}}
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var parent libfossil.FslID
	for i, files := range [][]manifest.File{
		{{Name: "src/keep.c", Content: []byte("keep\n")}, {Name: "src/old.c", Content: []byte("old 1\n")}},
		{{Name: "src/keep.c", Content: []byte("keep\n")}, {Name: "src/old.c", Content: []byte("old 2\n")}},
		{{Name: "src/keep.c", Content: []byte("keep\n")}, {Name: "src/new.c", Content: []byte("new\n")}},
	} {
		when = when.Add(time.Hour)
		rid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
			Files: files, Comment: "c", User: "u", Parent: parent, Time: when,
		})
		if err != nil {
			t.Fatal(err)
		}
		parent = rid
		switch i {
		case 0:
			f.c1 = rid
		case 1:
			f.c2 = rid
		case 2:
			f.c3 = rid
		}
	}
	return f
}
