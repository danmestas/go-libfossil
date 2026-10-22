package manifest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

func partialMetadataFixture(t *testing.T) (*treeFixture, libfossil.FslID, string, []byte) {
	t.Helper()
	f := newTreeFixture(t)
	outside := f.leaf(f.file("vendor/a"))
	root := treeDeck(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), []deck.SubmanifestCard{f.leaf(f.file("src/a")), outside})
	root.T = []deck.TagCard{{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"}}
	uuid := f.store(root)
	rid, _ := content.AvailableByUUID(f.r.DB(), uuid)
	node, _ := content.AvailableByUUID(f.r.DB(), outside.UUID)
	payload, err := content.Expand(f.r.DB(), node)
	if err != nil {
		t.Fatal(err)
	}
	elideWriterNode(t, f.r, outside.UUID)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.r.DB(), c); err != nil {
		t.Fatal(err)
	}
	return f, rid, outside.UUID, payload
}

func TestSparseMetadataRebuildAndRollback(t *testing.T) {
	f, rid, outside, payload := partialMetadataFixture(t)
	if _, err := Crosslink(f.r); err != nil {
		t.Fatal(err)
	}
	assertSparseRows(t, f, int64(rid), 1, 0, 1)
	if err := f.r.WithTx(func(tx *db.Tx) error {
		for _, table := range []string{"event", "plink", "mlink", "tagxref", "leaf", "orphan"} {
			if _, err := tx.Exec("DELETE FROM " + table); err != nil {
				return err
			}
		}
		_, err := CrosslinkRebuild(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertSparseRows(t, f, int64(rid), 1, 0, 1)
	rollback := errors.New("rollback after complete derivation")
	err := f.r.WithTx(func(tx *db.Tx) error {
		if _, _, err := blob.Store(tx, payload); err != nil {
			return err
		}
		if _, err := CrosslinkRebuild(tx); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM mlink WHERE mid=?", rid).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("completion inside rollback transaction = %d mlinks, want 2", n)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback = %v", err)
	}
	assertSparseRows(t, f, int64(rid), 1, 0, 1)
	if _, ok := content.AvailableByUUID(f.r.DB(), outside); ok {
		t.Fatal("rolled-back node arrival leaked")
	}
	f.storeBytes(payload)
	if _, err := Crosslink(f.r); err != nil {
		t.Fatal(err)
	}
	assertSparseRows(t, f, int64(rid), 1, 2, 0)
	want := dumpDerivedRows(t, f.r)
	if err := f.r.WithTx(func(tx *db.Tx) error {
		for _, table := range []string{"event", "plink", "mlink", "tagxref", "leaf", "orphan"} {
			if _, err := tx.Exec("DELETE FROM " + table); err != nil {
				return err
			}
		}
		_, err := CrosslinkRebuild(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := dumpDerivedRows(t, f.r); got != want {
		t.Fatalf("dense rebuild diverged:\ngot:\n%s\nwant:\n%s", got, want)
	}
	assertSparseRows(t, f, int64(rid), 1, 2, 0)
}

func TestSparseMetadataRejectsLaterNeededGap(t *testing.T) {
	f := newTreeFixture(t)
	refs := []deck.SubmanifestCard{f.leaf(f.file("src/a")), f.leaf(f.file("vendor/a")), f.leaf(f.file("z/a"))}
	uuid := f.store(treeDeck(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), refs))
	for _, ref := range refs[1:] {
		elideWriterNode(t, f.r, ref.UUID)
	}
	c, err := cone.New([]string{"src", "z"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.r.DB(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := Crosslink(f.r); err != nil {
		t.Fatal(err)
	}
	rid, _ := content.AvailableByUUID(f.r.DB(), uuid)
	var n int
	if err := f.r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("outside first gap concealed a later needed gap")
	}
}

// TestSparseRebuildLoadsReferencesAfterTheSweep pins the order a real-sized
// rebuild depends on: whole-store references are answered from the crosslink
// tables, so they must be loaded once the sweep has filled them. Loaded
// mid-sweep they find nothing to answer from and read every blob raw, which
// exceeds the reference budget on a real repository.
func TestSparseRebuildLoadsReferencesAfterTheSweep(t *testing.T) {
	f, _, _, _ := partialMetadataFixture(t)
	if _, err := Crosslink(f.r); err != nil {
		t.Fatal(err)
	}
	var nonArtifacts, loads int
	load := loadConeReferences
	loadConeReferences = func(ctx context.Context, q db.Querier, c *cone.Cone) (*content.ConeReferences, error) {
		loads++
		err := q.QueryRow("SELECT count(*) FROM " + nonArtifactTable).Scan(&nonArtifacts)
		if err != nil {
			t.Error(err)
		}
		return load(ctx, q, c)
	}
	t.Cleanup(func() { loadConeReferences = load })
	if _, err := f.r.DB().Exec("DELETE FROM " + nonArtifactTable); err != nil {
		t.Fatal(err)
	}
	if err := f.r.WithTx(func(tx *db.Tx) error {
		for _, table := range []string{"event", "plink", "mlink", "tagxref", "leaf", "orphan"} {
			if _, err := tx.Exec("DELETE FROM " + table); err != nil {
				return err
			}
		}
		_, err := CrosslinkRebuild(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if loads == 0 {
		t.Fatal("rebuild never loaded references: the fixture no longer defers a check-in")
	}
	if nonArtifacts == 0 {
		t.Fatal("references were loaded before the sweep recorded the file blobs it examined")
	}
}
