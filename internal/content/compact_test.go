package content

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

func compactFixture(t *testing.T) (*db.DB, libfossil.FslID, libfossil.FslID, libfossil.FslID) {
	t.Helper()
	d := setupTestDB(t)
	out, outName, err := blob.Store(d, []byte("outside file bytes"))
	if err != nil {
		t.Fatal(err)
	}
	in, inName, err := blob.Store(d, []byte("inside file bytes"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := (&deck.Deck{Type: deck.Checkin, D: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), F: []deck.FileCard{{Name: "docs/a", UUID: outName}, {Name: "src/a", UUID: inName}}, U: deck.User("test")}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	spine, _, err := blob.Store(d, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetStoreCone(d, mustCone(t, []string{"src"}, nil)); err != nil {
		t.Fatal(err)
	}
	return d, out, in, spine
}

func TestCompactStoreNarrowThenCompact(t *testing.T) {
	d, out, in, spine := compactFixture(t)
	for _, rid := range []libfossil.FslID{out, in, spine} {
		if _, err := Expand(d, rid); err != nil {
			t.Fatal("narrow deleted content", err)
		}
	}
	n, err := CompactStore(context.Background(), d)
	if err != nil || n != 1 {
		t.Fatalf("CompactStore = %d, %v; want 1, nil", n, err)
	}
	var size int
	var uuid string
	if err := d.QueryRow("SELECT size, uuid FROM blob WHERE rid=?", out).Scan(&size, &uuid); err != nil || size != -1 || uuid == "" {
		t.Fatalf("identity lost: %d %q %v", size, uuid, err)
	}
	var count int
	if err := d.QueryRow("SELECT count(*) FROM phantom WHERE rid=?", out).Scan(&count); err != nil || count != 1 {
		t.Fatalf("phantom = %d, %v", count, err)
	}
	for _, rid := range []libfossil.FslID{in, spine} {
		if _, err := Expand(d, rid); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := CompactStore(context.Background(), d); err != nil || n != 0 {
		t.Fatalf("repeat = %d, %v", n, err)
	}
}

func TestCompactStoreDenseAndCancelled(t *testing.T) {
	d, out, _, _ := compactFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CompactStore(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	if err := SetStoreCone(d, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := CompactStore(context.Background(), d); err != nil || n != 0 {
		t.Fatalf("dense = %d, %v", n, err)
	}
	if _, err := Expand(d, out); err != nil {
		t.Fatal(err)
	}
}

func TestCompactStoreRollsBack(t *testing.T) {
	d, out, _, _ := compactFixture(t)
	if _, err := d.Exec("CREATE TRIGGER refuse_phantom BEFORE INSERT ON phantom BEGIN SELECT RAISE(ABORT, 'refuse'); END"); err != nil {
		t.Fatal(err)
	}
	if n, err := CompactStore(context.Background(), d); err == nil || n != 0 {
		t.Fatalf("failure = %d, %v", n, err)
	}
	if _, err := Expand(d, out); err != nil {
		t.Fatal("failed compact lost bytes", err)
	}
}
