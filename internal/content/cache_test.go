package content

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestCache_HitMiss(t *testing.T) {
	d := setupTestDB(t)

	data := []byte("hello cache")
	rid, _, err := blob.Store(d, data)
	if err != nil {
		t.Fatal(err)
	}

	c := NewCache(1 << 20) // 1MB

	// First call = miss
	got, err := c.Expand(d, rid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}

	stats := c.Stats()
	if stats.Hits != 0 || stats.Misses != 1 {
		t.Fatalf("expected 0 hits / 1 miss, got %d / %d", stats.Hits, stats.Misses)
	}

	// Second call = hit
	got2, err := c.Expand(d, rid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, data) {
		t.Fatalf("got %q, want %q", got2, data)
	}

	stats = c.Stats()
	if stats.Hits != 1 || stats.Misses != 1 {
		t.Fatalf("expected 1 hit / 1 miss, got %d / %d", stats.Hits, stats.Misses)
	}
}

func TestCache_ReturnsCopy(t *testing.T) {
	d := setupTestDB(t)

	data := []byte("immutable data")
	rid, _, _ := blob.Store(d, data)

	c := NewCache(1 << 20)

	got1, _ := c.Expand(d, rid)
	got1[0] = 'X' // mutate the returned slice

	got2, _ := c.Expand(d, rid)
	if got2[0] == 'X' {
		t.Fatal("cache returned a reference instead of a copy — caller mutation corrupted cache")
	}
}

func TestCache_Eviction(t *testing.T) {
	d := setupTestDB(t)

	// Create blobs: 100 bytes each. Cache fits ~2.
	data1 := bytes.Repeat([]byte("A"), 100)
	data2 := bytes.Repeat([]byte("B"), 100)
	data3 := bytes.Repeat([]byte("C"), 100)

	rid1, _, _ := blob.Store(d, data1)
	rid2, _, _ := blob.Store(d, data2)
	rid3, _, _ := blob.Store(d, data3)

	c := NewCache(250) // fits ~2 entries of 100 bytes each

	c.Expand(d, rid1)
	c.Expand(d, rid2)

	stats := c.Stats()
	if stats.Entries != 2 {
		t.Fatalf("expected 2 entries, got %d", stats.Entries)
	}

	// Adding rid3 should evict rid1 (LRU)
	c.Expand(d, rid3)

	stats = c.Stats()
	if stats.Entries != 2 {
		t.Fatalf("expected 2 entries after eviction, got %d", stats.Entries)
	}
	if stats.Size > 250 {
		t.Fatalf("cache size %d exceeds max 250", stats.Size)
	}
}

func TestCache_Invalidate(t *testing.T) {
	d := setupTestDB(t)

	rid, _, _ := blob.Store(d, []byte("data to invalidate"))

	c := NewCache(1 << 20)
	c.Expand(d, rid)

	if c.Stats().Entries != 1 {
		t.Fatal("expected 1 entry")
	}

	c.Invalidate(rid)

	if c.Stats().Entries != 0 {
		t.Fatal("expected 0 entries after invalidate")
	}
}

func TestCache_Clear(t *testing.T) {
	d := setupTestDB(t)

	rid, _, _ := blob.Store(d, []byte("data to clear"))

	c := NewCache(1 << 20)
	c.Expand(d, rid)
	c.Clear()

	stats := c.Stats()
	if stats.Entries != 0 || stats.Size != 0 {
		t.Fatalf("expected empty cache, got entries=%d size=%d", stats.Entries, stats.Size)
	}
}

func TestCache_NilPassthrough(t *testing.T) {
	d := setupTestDB(t)

	data := []byte("passthrough data")
	rid, _, _ := blob.Store(d, data)

	var c *Cache
	got, err := c.Expand(d, rid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}
}

func TestCache_ConcurrentAccess(t *testing.T) {
	d := setupTestDB(t)

	data := []byte("concurrent data")
	rid, _, _ := blob.Store(d, data)

	c := NewCache(1 << 20)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.Expand(d, rid)
			if err != nil {
				t.Errorf("expand error: %v", err)
				return
			}
			if !bytes.Equal(got, data) {
				t.Errorf("got %q, want %q", got, data)
			}
		}()
	}
	wg.Wait()
}

func TestNewCache_PanicsOnZero(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for maxBytes <= 0")
		}
	}()
	NewCache(0)
}

func TestCache_LRUOrder(t *testing.T) {
	d := setupTestDB(t)

	data := bytes.Repeat([]byte("X"), 100)
	rid1, _, _ := blob.Store(d, data)

	data2 := bytes.Repeat([]byte("Y"), 100)
	rid2, _, _ := blob.Store(d, data2)

	data3 := bytes.Repeat([]byte("Z"), 100)
	rid3, _, _ := blob.Store(d, data3)

	c := NewCache(250) // fits ~2 entries

	c.Expand(d, rid1) // order: [rid1]
	c.Expand(d, rid2) // order: [rid2, rid1]
	c.Expand(d, rid1) // touch rid1 → order: [rid1, rid2]
	c.Expand(d, rid3) // evicts rid2 (LRU), order: [rid3, rid1]

	// rid1 should still be cached (was touched)
	missesBefore := c.Stats().Misses
	c.Expand(d, rid1)
	if c.Stats().Misses != missesBefore {
		t.Fatal("rid1 should have been a cache hit (was recently touched)")
	}

	// rid2 should have been evicted
	c.Expand(d, rid2)
	if c.Stats().Misses != missesBefore+1 {
		t.Fatal("rid2 should have been a cache miss (was evicted)")
	}
}

func TestCache_BlobLargerThanMax(t *testing.T) {
	d := setupTestDB(t)

	// 500-byte blob into a 100-byte cache — blob exceeds maxSize
	big := bytes.Repeat([]byte("B"), 500)
	rid, _, _ := blob.Store(d, big)

	c := NewCache(100)

	// The data should still be returned correctly even though it can't stay cached.
	got, err := c.Expand(d, rid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("data mismatch for oversized blob")
	}

	// The blob is inserted then immediately evicted (size 500 > max 100).
	stats := c.Stats()
	if stats.Entries != 0 {
		t.Fatalf("expected 0 entries (oversized blob evicted), got %d", stats.Entries)
	}
}

func TestCache_ExpandError(t *testing.T) {
	d := setupTestDB(t)

	c := NewCache(1 << 20)

	// rid 99999 doesn't exist — Expand should fail
	_, err := c.Expand(d, 99999)
	if err == nil {
		t.Fatal("expected error for nonexistent rid")
	}

	// Cache should remain empty — error results must not be cached
	stats := c.Stats()
	if stats.Entries != 0 {
		t.Fatalf("expected 0 entries after failed expand, got %d", stats.Entries)
	}
}

func TestCache_DeltaChainViaCacheHit(t *testing.T) {
	d := setupTestDB(t)

	// Build a 3-deep delta chain and verify cached expansion is correct
	v1 := []byte("version one with enough padding to create proper deltas here!!")
	v2 := []byte("version TWO with enough padding to create proper deltas here!!")
	v3 := []byte("version THREE with enough padding to create proper deltas here!!")

	rid1, _, _ := blob.Store(d, v1)
	rid2, _, _ := blob.StoreDelta(d, v2, rid1)
	rid3, _, _ := blob.StoreDelta(d, v3, rid2)

	c := NewCache(1 << 20)

	// Expand the leaf of the chain (miss)
	got, err := c.Expand(d, rid3)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, v3) {
		t.Fatalf("got %q, want %q", got, v3)
	}

	// Second expand should hit cache and return identical data
	got2, err := c.Expand(d, rid3)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, v3) {
		t.Fatalf("cached result differs: got %q, want %q", got2, v3)
	}

	stats := c.Stats()
	if stats.Hits != 1 || stats.Misses != 1 {
		t.Fatalf("expected 1 hit / 1 miss, got %d / %d", stats.Hits, stats.Misses)
	}
}

func BenchmarkCache_Expand(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.fossil")
	d, _ := db.Open(path)
	db.CreateRepoSchema(d)
	defer d.Close()

	base := bytes.Repeat([]byte("base content for cache benchmark "), 100)
	rid, _, _ := blob.Store(d, base)

	// Build a 5-deep delta chain
	for i := 0; i < 5; i++ {
		next := make([]byte, len(base))
		copy(next, base)
		copy(next[i*50:], []byte("CHANGED!"))
		newRid, _, _ := blob.StoreDelta(d, next, rid)
		rid = newRid
		base = next
	}

	c := NewCache(1 << 20)

	// Prime the cache
	c.Expand(d, rid)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Expand(d, rid)
	}
}
