package content

import (
	"bytes"
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

func chunkBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	_, _ = rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// partsOf counts rid's blobpart rows.
func partsOf(t *testing.T, q db.Querier, rid libfossil.FslID) int {
	t.Helper()
	var n int
	if err := q.QueryRow("SELECT count(*) FROM blobpart WHERE rid=?", rid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func expandEquals(t *testing.T, q db.Querier, rid libfossil.FslID, want []byte) {
	t.Helper()
	got, err := Expand(q, rid)
	if err != nil {
		t.Fatalf("Expand rid=%d: %v", rid, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Expand rid=%d: content differs", rid)
	}
}

// Deltifying a chunked row rewrites it inline and drops its parts; undoing
// that chunks it again.
func TestDeltifyAndUndeltaChunkedRow(t *testing.T) {
	t.Cleanup(blob.SetMaxValueBytes(1_990_000))
	d := setupTestDB(t)
	old := chunkBytes(2_100_000, 1)
	cur := bytes.Clone(old)
	copy(cur[500_000:], "an edit")
	oldRid, _, err := blob.Store(d, old)
	if err != nil {
		t.Fatal(err)
	}
	curRid, _, err := blob.Store(d, cur)
	if err != nil {
		t.Fatal(err)
	}
	if partsOf(t, d, oldRid) != 3 {
		t.Fatalf("fixture: rid %d is not chunked", oldRid)
	}
	err = d.WithTx(func(tx *db.Tx) error {
		saved, err := Deltify(tx, oldRid, curRid)
		if err == nil && saved <= 2_000_000 {
			t.Errorf("Deltify saved %d bytes, want the chunked value's worth", saved)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if partsOf(t, d, oldRid) != 0 {
		t.Fatal("Deltify kept the rewritten row's parts")
	}
	expandEquals(t, d, oldRid, old)

	if err := d.WithTx(func(tx *db.Tx) error { return Undelta(tx, oldRid) }); err != nil {
		t.Fatal(err)
	}
	if partsOf(t, d, oldRid) != 3 {
		t.Fatal("Undelta did not chunk the full value")
	}
	expandEquals(t, d, oldRid, old)
	expandEquals(t, d, curRid, cur)
}

// Compaction turns a chunked row outside the cone into a phantom and takes
// its parts with it.
func TestCompactStoreDropsParts(t *testing.T) {
	t.Cleanup(blob.SetMaxValueBytes(1_990_000))
	d := setupTestDB(t)
	out, outName, err := blob.Store(d, chunkBytes(2_100_000, 2))
	if err != nil {
		t.Fatal(err)
	}
	inside := chunkBytes(2_100_000, 3)
	in, inName, err := blob.Store(d, inside)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := (&deck.Deck{Type: deck.Checkin, D: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		F: []deck.FileCard{{Name: "docs/a", UUID: outName}, {Name: "src/a", UUID: inName}},
		U: deck.User("test")}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := blob.Store(d, manifest); err != nil {
		t.Fatal(err)
	}
	if err := SetStoreCone(d, mustCone(t, []string{"src"}, nil)); err != nil {
		t.Fatal(err)
	}
	if n, err := CompactStore(context.Background(), d); err != nil || n != 1 {
		t.Fatalf("CompactStore = %d, %v; want 1, nil", n, err)
	}
	if partsOf(t, d, out) != 0 {
		t.Fatal("compaction kept the dropped row's parts")
	}
	if _, err := blob.ReadStored(d, out); err == nil {
		t.Fatal("compacted row still reads")
	}
	if partsOf(t, d, in) != 3 {
		t.Fatal("compaction touched the kept row's parts")
	}
	expandEquals(t, d, in, inside)
}
