package content

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

func TestConeReferencesRawPartialTree(t *testing.T) {
	d := setupTestDB(t)
	c := mustCone(t, []string{"src"}, nil)
	outUUID := strings.Repeat("a", 64)
	out, err := blob.StorePhantom(d, outUUID)
	if err != nil {
		t.Fatal(err)
	}
	insideUUID := strings.Repeat("b", 64)
	inside, err := blob.StorePhantom(d, insideUUID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := (&deck.Deck{C: "partial", D: time.Now().UTC(), U: deck.User("u"), S: []deck.SubmanifestCard{{Name: "src/a", UUID: insideUUID}, {Name: "zzz/a", UUID: outUUID}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := blob.Store(d, data); err != nil {
		t.Fatal(err)
	}
	if err := SetStoreCone(d, c); err != nil {
		t.Fatal(err)
	}
	// Neither child is held and the spine has never been crosslinked.
	refs, err := LoadConeReferences(context.Background(), d, c)
	if err != nil {
		t.Fatal(err)
	}
	if !refs.Outside(out) || refs.Outside(inside) || !refs.Required(inside) {
		t.Fatal("raw S ranges were not classified")
	}
	a, err := LoadAbsences(d)
	if err != nil {
		t.Fatal(err)
	}
	if a.Class(out) != Elided || a.Class(inside) != Transit {
		t.Fatalf("out=%v in=%v", a.Class(out), a.Class(inside))
	}
	if err := SetStoreCone(d, nil); err != nil {
		t.Fatal(err)
	}
	a, err = LoadAbsences(d)
	if err != nil || a.Class(out) != Transit {
		t.Fatalf("policy loss: %v %v", a, err)
	}
}

func TestConeReferencesRetainProvenancePreservesPolicy(t *testing.T) {
	d := setupTestDB(t)
	leaf, err := (&deck.Deck{F: []deck.FileCard{{Name: "zzz/a", UUID: strings.Repeat("a", 40)}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	rid, uuid, err := blob.Store(d, leaf)
	if err != nil {
		t.Fatal(err)
	}
	root, err := (&deck.Deck{C: "outside leaf", D: time.Now().UTC(), U: deck.User("u"), S: []deck.SubmanifestCard{{Name: "zzz/a", UUID: uuid}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := blob.Store(d, root); err != nil {
		t.Fatal(err)
	}
	refs, err := LoadConeReferences(context.Background(), d, mustCone(t, []string{"src"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !refs.Outside(rid) || refs.Required(rid) {
		t.Fatal("outside leaf policy misclassified before retention")
	}
	drop := map[libfossil.FslID]bool{rid: true}
	if err := refs.RetainProvenance(context.Background(), drop); err != nil {
		t.Fatal(err)
	}
	if drop[rid] || !refs.Outside(rid) || refs.Required(rid) {
		t.Fatal("retaining raw proof changed policy instead of only the drop set")
	}
}

func TestConeReferenceWalkBounds(t *testing.T) {
	for _, tc := range []struct {
		name           string
		depth, visited int
		cyclic         bool
	}{
		{"depth", coneReferenceDepth + 1, 0, false},
		{"visited", 1, coneReferenceNodes, false},
		{"cycle", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &ConeReferences{outside: map[string]bool{}, required: map[string]bool{}, tree: map[string]bool{}}
			w := coneReferenceWalk{ctx: context.Background(), cone: mustCone(t, []string{"src"}, nil), refs: p, nodes: map[string]*deck.Deck{"node": {F: []deck.FileCard{{Name: "src/a"}}}}, path: map[string]bool{"node": tc.cyclic}, visited: tc.visited}
			if err := w.walk([]deck.SubmanifestCard{{Name: "src/a", UUID: "node"}}, "", tc.depth, true); err == nil {
				t.Fatal("unbounded walk accepted")
			}
		})
	}
}

func TestConeReferenceWalkRejectsEscapedRange(t *testing.T) {
	p := &ConeReferences{outside: map[string]bool{}, required: map[string]bool{}, tree: map[string]bool{}}
	w := coneReferenceWalk{ctx: context.Background(), cone: mustCone(t, []string{"src"}, nil), refs: p, nodes: map[string]*deck.Deck{"node": {F: []deck.FileCard{{Name: "out/a"}, {Name: "src/a"}}}}, path: map[string]bool{}}
	if err := w.walk([]deck.SubmanifestCard{{Name: "out/a", UUID: "node"}}, "src", 1, true); err == nil {
		t.Fatal("accepted leaf escaping its declared interval")
	}
}

func TestConeReferencesCancelled(t *testing.T) {
	d := setupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := LoadConeReferences(ctx, d, mustCone(t, []string{"src"}, nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := LoadAbsencesContext(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("absence cancellation: %v", err)
	}
}

func TestConeReferencesKeepsUnexpandable(t *testing.T) {
	d := setupTestDB(t)
	rid, uuid, err := blob.Store(d, []byte("damaged payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("UPDATE blob SET content=? WHERE rid=?", []byte("broken compression"), rid); err != nil {
		t.Fatal(err)
	}
	data, err := (&deck.Deck{C: "outside damaged", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "zzz/a", UUID: uuid}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := blob.Store(d, data); err != nil {
		t.Fatal(err)
	}
	refs, err := LoadConeReferences(context.Background(), d, mustCone(t, []string{"src"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if refs.Outside(rid) {
		t.Fatal("unexpandable potential structural artifact classified for deletion")
	}
}

func TestConeReferencesRawStructuralAlias(t *testing.T) {
	d := setupTestDB(t)
	c := mustCone(t, []string{"src"}, nil)
	uuid := strings.Repeat("c", 64)
	rid, err := blob.StorePhantom(d, uuid)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []*deck.Deck{
		{C: "outside", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "zzz/a", UUID: uuid}}},
		{C: "parent", D: time.Now().UTC(), U: deck.User("u"), P: []string{uuid}},
	} {
		data, err := artifact.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := blob.Store(d, data); err != nil {
			t.Fatal(err)
		}
	}
	refs, err := LoadConeReferences(context.Background(), d, c)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Outside(rid) || !refs.Required(rid) {
		t.Fatal("outside F hid raw structural alias")
	}
	if err := SetStoreCone(d, c); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAbsences(d)
	if err != nil || a.Class(libfossil.FslID(rid)) != Transit {
		t.Fatalf("alias classification: %v %v", a, err)
	}
}
