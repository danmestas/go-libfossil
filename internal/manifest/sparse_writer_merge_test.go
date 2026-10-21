package manifest

import (
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// Final-group merging can absorb an untouched preceding leaf even when no
// MAX cut is reached. It can also turn the whole check-in back into flat F.
func TestSparseWriterCanonicalFinalMergePropagation(t *testing.T) {
	var files []deck.FileCard
	for i := 0; i < 1024 && len(files) < 4; i++ {
		name := fmt.Sprintf("g%04d", i)
		boundary := zeroBits(name) >= 3
		if boundary != (len(files) == 2) {
			continue
		}
		files = append(files, deck.FileCard{Name: name, UUID: hash.SHA3([]byte(name))})
	}
	if len(files) != 4 {
		t.Fatal("bounded search found too few names")
	}
	before := mustBuildTree(t, files, 3)
	if len(before.S) != 2 {
		t.Fatalf("old S count %d, want two leaves", len(before.S))
	}
	for _, data := range before.Subs {
		leaf, err := deck.Parse(data)
		if err != nil || len(leaf.F) != 2 {
			t.Fatalf("old leaf = %v, %v; want two entries", leaf, err)
		}
	}
	if after := mustBuildTree(t, files[:3], 3); len(after.S) != 0 || len(after.Subs) != 0 {
		t.Fatalf("deleting %s built %d S cards; want flat after MIN merge", files[3].Name, len(after.S))
	}
	t.Logf("deleting %s in second leaf requires emitting the first leaf's two untouched F cards", files[3].Name)
}
