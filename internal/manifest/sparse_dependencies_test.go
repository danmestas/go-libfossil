package manifest

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
)

func TestCompleteChildWaitsForPartialTreeParent(t *testing.T) {
	f := newTreeFixture(t)
	inside := f.file("src/a")
	outside := f.leaf(f.file("vendor/a"))
	parent := f.store(treeDeck(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), []deck.SubmanifestCard{f.leaf(inside), outside}))
	child := f.store(&deck.Deck{C: "flat child", D: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), F: []deck.FileCard{inside}, P: []string{parent}, U: deck.User("u")})
	elideWriterNode(t, f.r, outside.UUID)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(f.r.DB(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := Crosslink(f.r); err != nil {
		t.Fatal(err)
	}
	rid, _ := content.AvailableByUUID(f.r.DB(), child)
	for _, query := range []string{"SELECT count(*) FROM event WHERE objid=?", "SELECT count(*) FROM mlink WHERE mid=?"} {
		var got int
		if err := f.r.DB().QueryRow(query, rid).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 0 {
			t.Fatalf("child with incomplete parent: %s = %d, want 0", query, got)
		}
	}
	var waiting int
	if err := f.r.DB().QueryRow("SELECT count(*) FROM orphan WHERE rid=?", rid).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if waiting != 1 {
		t.Fatalf("child has %d dependency markers, want 1", waiting)
	}
}
