package content

import (
	"context"
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

func TestConeSuccessorConeIntervalsAndOverrides(t *testing.T) {
	paths := []string{"src", "src-x", "src/a", "src/private/a", "src/private/open/a", "zzz/a"}
	facts := &ConeReferenceFacts{names: map[libfossil.FslID]string{}, held: map[string]libfossil.FslID{}}
	visible := make([]libfossil.FslID, len(paths)+1)
	manifest := &deck.Deck{}
	for i, path := range paths {
		rid := libfossil.FslID(i + 1)
		uuid := fmt.Sprintf("%040x", rid)
		facts.names[rid], facts.held[uuid], visible[i] = uuid, rid, rid
		manifest.F = append(manifest.F, deck.FileCard{Name: path, UUID: uuid})
	}
	// The outside artifact has an inside alias; structural references override
	// excluded aliases. The last RID is unknown and must remain dense.
	manifest.F = append(manifest.F, deck.FileCard{Name: "src/alias", UUID: facts.names[6]})
	manifest.B = facts.names[4]
	visible[6], facts.names[7] = 7, "unknown"
	facts.records = []coneReferenceRecord{{uuid: "manifest", deck: manifest}}
	index, err := NewConeSuccessors(context.Background(), facts, visible, 100, 10000)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{"src", "src/private/open"}, []string{"src/private"})
	if err != nil {
		t.Fatal(err)
	}
	query := index.Query(c)
	want := []libfossil.FslID{1, 3, 4, 5, 6, 7}
	first := libfossil.FslID(1)
	for _, expected := range want {
		got := query.Next(first, 7)
		if got != expected {
			t.Fatalf("next(%d)=%d want=%d", first, got, expected)
		}
		first = got + 1
	}
	if got := query.Next(first, 7); got != 0 {
		t.Fatalf("exhaustion returned %d", got)
	}
}

func TestConeSuccessorSkipsExcludedGap(t *testing.T) {
	facts := &ConeReferenceFacts{names: map[libfossil.FslID]string{}, held: map[string]libfossil.FslID{}}
	visible := make([]libfossil.FslID, 10001)
	manifest := &deck.Deck{}
	for i := range visible {
		rid := libfossil.FslID(i + 1)
		uuid := fmt.Sprintf("%040x", i+1)
		facts.names[rid], facts.held[uuid], visible[i] = uuid, rid, rid
		name := fmt.Sprintf("zzz/%05d", i)
		if i == len(visible)-1 {
			name = "src/final"
		}
		manifest.F = append(manifest.F, deck.FileCard{Name: name, UUID: uuid})
	}
	facts.records = []coneReferenceRecord{{uuid: "manifest", deck: manifest}}
	index, err := NewConeSuccessors(context.Background(), facts, visible, 20000, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	query := index.Query(c)
	if rid := query.Next(1, 20000); rid != 10001 {
		t.Fatalf("next eligible=%d", rid)
	}
	if query.Work.IndexVisits > 32 || query.Work.RIDComparisons > 64 {
		t.Fatalf("excluded gap was scanned: %+v", query.Work)
	}
	if _, err := NewConeSuccessors(context.Background(), facts, visible, 20000, 1); err == nil {
		t.Fatal("index allocation budget not enforced")
	}
}
