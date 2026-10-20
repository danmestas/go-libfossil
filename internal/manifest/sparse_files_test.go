package manifest

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
)

func TestFilteredDeltaOverElidedTree(t *testing.T) {
	f := newTreeFixture(t)
	inside := f.file("src/a")
	outside := f.file("vendor/a")
	left, right := f.leaf(inside), f.leaf(outside)
	base := f.store(treeDeck(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), []deck.SubmanifestCard{left, right}))
	replacement := f.file("src/b")
	delta := f.store(&deck.Deck{C: "delta", D: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), B: base,
		F: []deck.FileCard{{Name: inside.Name}, replacement}, U: deck.User("u")})
	elideWriterNode(t, f.r, right.UUID)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rid, ok := content.AvailableByUUID(f.r.DB(), delta)
	if !ok {
		t.Fatal("delta not stored")
	}
	files, err := ListFilesFiltered(f.r, rid, c)
	if err != nil || len(files) != 1 || files[0].Name != replacement.Name || files[0].UUID != replacement.UUID {
		t.Fatalf("filtered delta = %+v, %v; want replacement only", files, err)
	}
	if _, err := ListFiles(f.r, rid); err == nil {
		t.Fatal("dense enumeration succeeded without baseline subtree")
	} else if uuid, ok := MissingSubmanifest(err); !ok || uuid != right.UUID {
		t.Fatalf("dense enumeration = %v; want missing %s", err, right.UUID)
	}
}
