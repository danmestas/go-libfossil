package manifest

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
)

func TestSparseDeltaMetadataUsesEffectiveBaselineView(t *testing.T) {
	f := newTreeFixture(t)
	old := f.file("src/a")
	outside := f.leaf(f.file("vendor/a"))
	base := f.store(treeDeck(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), []deck.SubmanifestCard{f.leaf(old), outside}))
	replacement := f.file("src/b")
	delta := f.store(&deck.Deck{C: "delta", D: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), B: base,
		F: []deck.FileCard{{Name: old.Name}, replacement}, U: deck.User("u")})
	elideWriterNode(t, f.r, outside.UUID)
	elideWriterNode(t, f.r, old.UUID)
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
	rid, _ := content.AvailableByUUID(f.r.DB(), delta)
	var events, mlinks int
	if err := f.r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := f.r.DB().QueryRow("SELECT count(*) FROM mlink WHERE mid=?", rid).Scan(&mlinks); err != nil {
		t.Fatal(err)
	}
	if events != 1 || mlinks != 0 {
		t.Fatalf("effective delta metadata events=%d mlinks=%d, want 1/0", events, mlinks)
	}
}

func TestSparseMetadataRefusesInvalidDependencies(t *testing.T) {
	for _, kind := range []string{"parent", "baseline", "cherrypick"} {
		t.Run(kind, func(t *testing.T) {
			f := newTreeFixture(t)
			inside := f.file("src/a")
			outside := f.leaf(f.file("vendor/a"))
			d := treeDeck(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), []deck.SubmanifestCard{f.leaf(inside), outside})
			invalid := f.storeBytes([]byte("not a check-in"))
			switch kind {
			case "parent":
				d.P = []string{invalid}
			case "baseline":
				d.S = nil
				d.F = []deck.FileCard{inside}
				d.B = invalid
			case "cherrypick":
				d.Q = []deck.CherryPick{{Target: invalid}}
			}
			uuid := f.store(d)
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
			rid, _ := content.AvailableByUUID(f.r.DB(), uuid)
			var events int
			if err := f.r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if events != 0 {
				t.Fatal("invalid dependency allowed metadata publication")
			}
		})
	}
}
