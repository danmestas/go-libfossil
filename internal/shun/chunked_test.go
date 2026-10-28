package shun_test

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/shun"
)

// Purging a chunked base deletes its parts, and its dependent, rewritten as
// full content, is chunked in turn.
func TestPurgeChunked(t *testing.T) {
	t.Cleanup(blob.SetMaxValueBytes(1_990_000))
	d := setupDB(t)
	base := make([]byte, 2_100_000)
	_, _ = rand.New(rand.NewSource(1)).Read(base)
	dependent := bytes.Clone(base)
	copy(dependent[100:], "dependent")
	ridA, uuidA, err := blob.Store(d, base)
	if err != nil {
		t.Fatal(err)
	}
	ridB, _, err := blob.StoreDelta(d, dependent, ridA)
	if err != nil {
		t.Fatal(err)
	}
	if err := shun.Add(d, uuidA, "purge chunked"); err != nil {
		t.Fatal(err)
	}
	if _, err := shun.Purge(d); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	var parts, partsB int
	if err := d.QueryRow("SELECT count(*), count(*) FILTER (WHERE rid=?) FROM blobpart", ridB).
		Scan(&parts, &partsB); err != nil {
		t.Fatal(err)
	}
	if parts != 3 || partsB != 3 {
		t.Fatalf("parts = %d (dependent %d), want only the dependent's 3", parts, partsB)
	}
	got, err := content.Expand(d, ridB)
	if err != nil || !bytes.Equal(got, dependent) {
		t.Fatalf("Expand dependent: %v", err)
	}
}
