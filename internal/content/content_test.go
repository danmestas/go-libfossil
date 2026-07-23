package content

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func setupTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.CreateRepoSchema(d); err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestExpand_FullText(t *testing.T) {
	d := setupTestDB(t)
	content := []byte("full text content, no delta")

	rid, _, err := blob.Store(d, content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, err := Expand(d, rid)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("Expand = %q, want %q", got, content)
	}
}

func TestExpand_SingleDelta(t *testing.T) {
	d := setupTestDB(t)
	source := []byte("the original source content for delta testing purposes here")
	target := []byte("the original source content for MODIFIED testing purposes here")

	srcRid, _, err := blob.Store(d, source)
	if err != nil {
		t.Fatalf("Store source: %v", err)
	}

	tgtRid, _, err := blob.StoreDelta(d, target, srcRid)
	if err != nil {
		t.Fatalf("StoreDelta: %v", err)
	}

	got, err := Expand(d, tgtRid)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("Expand = %q, want %q", got, target)
	}
}

func TestExpand_DeltaChain(t *testing.T) {
	d := setupTestDB(t)
	v1 := []byte("version one of the content with enough data to make deltas work well")
	v2 := []byte("version TWO of the content with enough data to make deltas work well")
	v3 := []byte("version THREE of the content with enough data to make deltas work well")

	rid1, _, _ := blob.Store(d, v1)
	rid2, _, _ := blob.StoreDelta(d, v2, rid1)
	rid3, _, _ := blob.StoreDelta(d, v3, rid2)

	got, err := Expand(d, rid3)
	if err != nil {
		t.Fatalf("Expand chain: %v", err)
	}
	if !bytes.Equal(got, v3) {
		t.Fatalf("Expand chain = %q, want %q", got, v3)
	}
}

func TestExpand_InvalidRid(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for rid <= 0")
		}
	}()
	d := setupTestDB(t)
	Expand(d, 0)
}

func TestVerify(t *testing.T) {
	d := setupTestDB(t)
	content := []byte("content to verify")
	rid, _, _ := blob.Store(d, content)

	err := Verify(d, rid)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func BenchmarkExpand_DeltaChain(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.fossil")
	d, _ := db.Open(path)
	db.CreateRepoSchema(d)
	defer d.Close()

	base := bytes.Repeat([]byte("base content for benchmark "), 100)
	rid, _, _ := blob.Store(d, base)

	for i := 0; i < 5; i++ {
		next := make([]byte, len(base))
		copy(next, base)
		copy(next[i*50:], []byte("CHANGED!"))
		newRid, _, _ := blob.StoreDelta(d, next, rid)
		rid = newRid
		base = next
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Expand(d, rid)
	}
}
