package content

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// This tree has never crosslinked: a healthy missing outside S sibling keeps
// derived file indexes empty. Compaction must still reclaim proven outside
// bytes without losing the raw provenance that keeps new phantoms healthy.
func TestCompactStoreHealthyPartialTree(t *testing.T) {
	d := setupTestDB(t)
	in, inName, err := blob.Store(d, []byte("inside partial tree"))
	if err != nil {
		t.Fatal(err)
	}
	out, outName, err := blob.Store(d, []byte("outside partial tree"))
	if err != nil {
		t.Fatal(err)
	}
	inLeaf := compactDeck(t, d, &deck.Deck{Type: deck.Submanifest, F: []deck.FileCard{{Name: "src/a", UUID: inName}}})
	outLeaf := compactDeck(t, d, &deck.Deck{Type: deck.Submanifest, F: []deck.FileCard{{Name: "zzz/a", UUID: outName}}})
	outInterior := compactDeck(t, d, &deck.Deck{Type: deck.Submanifest, S: []deck.SubmanifestCard{{Name: "zzz/a", UUID: compactName(t, d, outLeaf)}}})
	phantom, err := blob.StorePhantom(d, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	spine := compactDeck(t, d, &deck.Deck{Type: deck.Checkin, D: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), S: []deck.SubmanifestCard{
		{Name: "src/a", UUID: compactName(t, d, inLeaf)},
		{Name: "zzz/a", UUID: compactName(t, d, outInterior)},
		{Name: "zzzz/a", UUID: compactName(t, d, phantom)},
	}})
	if err := SetStoreCone(d, mustCone(t, []string{"src"}, nil)); err != nil {
		t.Fatal(err)
	}
	// Only the outside blob goes: sparse-cone §5.4 keeps every held node,
	// the outside interior included.
	if n, err := CompactStore(context.Background(), d); err != nil || n != 1 {
		t.Fatalf("partial-tree compact = %d, %v; want 1, nil", n, err)
	}
	for _, rid := range []libfossil.FslID{in, inLeaf, outLeaf, outInterior, spine} {
		if _, err := Expand(d, rid); err != nil {
			t.Fatalf("required/provenance artifact %d lost: %v", rid, err)
		}
	}
	a, err := LoadAbsences(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, rid := range []libfossil.FslID{out, phantom} {
		if got := a.Class(rid); got != Elided {
			t.Errorf("Class(%d) = %v, want Elided", rid, got)
		}
	}
	if n, err := CompactStore(context.Background(), d); err != nil || n != 0 {
		t.Fatalf("repeat partial compact = %d, %v", n, err)
	}
}

// A missing nested S child's only range proof includes every held ancestor.
// Keeping its immediate parent alone is insufficient if the root cannot reach it.
func TestCompactStoreRetainsMissingDescendantProvenance(t *testing.T) {
	d, out, _, _ := compactFixture(t)
	missingName := strings.Repeat("b", 40)
	missing, err := blob.StorePhantom(d, missingName)
	if err != nil {
		t.Fatal(err)
	}
	child := compactDeck(t, d, &deck.Deck{Type: deck.Submanifest, S: []deck.SubmanifestCard{{Name: "zzz/a", UUID: missingName}}})
	parent := compactDeck(t, d, &deck.Deck{Type: deck.Submanifest, S: []deck.SubmanifestCard{{Name: "zzz/a", UUID: compactName(t, d, child)}}})
	compactDeck(t, d, &deck.Deck{Type: deck.Checkin, D: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), S: []deck.SubmanifestCard{{Name: "zzz/a", UUID: compactName(t, d, parent)}}})
	if n, err := CompactStore(context.Background(), d); err != nil || n != 1 {
		t.Fatalf("compact = %d, %v; want file only", n, err)
	}
	for _, rid := range []libfossil.FslID{child, parent} {
		if _, err := Expand(d, rid); err != nil {
			t.Fatal("lost nested range provenance", err)
		}
	}
	a, err := LoadAbsences(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, rid := range []libfossil.FslID{out, missing} {
		if a.Class(rid) != Elided {
			t.Fatalf("Class(%d) = %v, want Elided", rid, a.Class(rid))
		}
	}
}

// Stale derived links cannot manufacture permission to delete a covered blob,
// or make an unrelated, unclassified artifact into a compaction candidate.
func TestCompactStoreIgnoresStaleDerivedIndexes(t *testing.T) {
	d, out, in, _ := compactFixture(t)
	unknown, _, err := blob.Store(d, []byte("unknown held content"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO filename(fnid,name) VALUES(1,'docs/stale')"); err != nil {
		t.Fatal(err)
	}
	for _, rid := range []libfossil.FslID{in, unknown} {
		if _, err := d.Exec("INSERT INTO mlink(mid,fid,fnid) VALUES(999,?,1)", rid); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := CompactStore(context.Background(), d); err != nil || n != 1 {
		t.Fatalf("stale-index compact = %d, %v", n, err)
	}
	for _, rid := range []libfossil.FslID{in, unknown} {
		if _, err := Expand(d, rid); err != nil {
			t.Fatal("stale index caused deletion", err)
		}
	}
	var size int
	if err := d.QueryRow("SELECT size FROM blob WHERE rid=?", out).Scan(&size); err != nil || size != -1 {
		t.Fatalf("outside size = %d, %v", size, err)
	}
}
