package content

import (
	"container/list"
	"sync"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/db"
)

// Cache is a concurrency-safe LRU cache for expanded blob content.
// It reduces redundant delta-chain walks by caching the fully-expanded
// result of [Expand], keyed by rid.
//
// A nil *Cache is valid and acts as a passthrough to [Expand].
type Cache struct {
	mu      sync.Mutex
	items   map[libfossil.FslID]*list.Element
	order   *list.List
	curSize int64
	maxSize int64
	hits    int64
	misses  int64
}

type cacheEntry struct {
	rid  libfossil.FslID
	data []byte
}

// CacheStats reports cache hit/miss statistics and current memory usage.
type CacheStats struct {
	Hits    int64
	Misses  int64
	Size    int64
	MaxSize int64
	Entries int
}

// NewCache creates a cache bounded by maxBytes of expanded content.
func NewCache(maxBytes int64) *Cache {
	if maxBytes <= 0 {
		panic("content.NewCache: maxBytes must be > 0")
	}
	return &Cache{
		items:   make(map[libfossil.FslID]*list.Element),
		order:   list.New(),
		maxSize: maxBytes,
	}
}

// Expand returns the expanded content for rid, serving from cache when possible.
//
// On a miss it walks rid's delta chain only as far back as the deepest
// ancestor the cache already holds, and caches every node it materializes on
// the way forward. That is what makes a sweep over a whole repository — every
// blob of a chain expanded once, in some order — cost one delta application
// per blob rather than one per (blob, chain-depth) pair.
//
// A nil receiver falls through to [Expand] directly.
func (c *Cache) Expand(q db.Querier, rid libfossil.FslID) ([]byte, error) {
	if c == nil {
		return Expand(q, rid)
	}
	if q == nil {
		panic("content.Cache.Expand: q must not be nil")
	}
	if rid <= 0 {
		panic("content.Cache.Expand: rid must be > 0")
	}

	c.mu.Lock()
	if elem, ok := c.items[rid]; ok {
		c.order.MoveToFront(elem)
		data := elem.Value.(*cacheEntry).data
		c.hits++
		c.mu.Unlock()
		out := make([]byte, len(data))
		copy(out, data)
		return out, nil
	}
	c.misses++
	c.mu.Unlock()

	// have hands out the cache's own buffer; expandChain never writes
	// through it, and the copy below is what the caller gets.
	have := func(id libfossil.FslID) ([]byte, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		elem, ok := c.items[id]
		if !ok {
			return nil, false
		}
		c.order.MoveToFront(elem)
		return elem.Value.(*cacheEntry).data, true
	}

	data, err := expandChain(q, rid, have, c.store)
	if err != nil {
		return nil, err
	}

	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// store takes ownership of data as the cached content for rid. expandChain
// hands over buffers it will not touch again.
func (c *Cache) store(rid libfossil.FslID, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[rid]; ok {
		c.order.MoveToFront(elem)
		return
	}

	data = clip(data)
	elem := c.order.PushFront(&cacheEntry{rid: rid, data: data})
	c.items[rid] = elem
	c.curSize += entrySize(data)

	for c.curSize > c.maxSize && c.order.Len() > 0 {
		c.evictOldest()
	}
}

// clip returns data in a buffer with no spare capacity, copying only when
// there is slack to reclaim.
//
// Expanded content arrives from delta.Apply's append growth, whose capacity
// is rounded up to a size class. Accounting a budget in lengths while
// retaining capacities lets maxSize drift from the bytes actually held, by an
// amount that depends on the allocator rather than on anything the caller
// controls. Clipping makes curSize exact instead of approximate.
//
// It is an accounting fix, not a memory saving, and it was measured: against
// the Fossil SCM repository the cache holds the same 543 entries and the same
// 255 MiB at a 256 MiB budget with and without it, because at those sizes the
// buffers already arrive tightly sized. The slack is real for small entries --
// it is what the eviction tests exercise -- and negligible for large ones.
// Peak process RSS did not move either (1067 MB against 1050 MB); see
// manifest.crosslinkCacheBytes for where that number actually comes from.
func clip(data []byte) []byte {
	if cap(data) == len(data) {
		return data
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out
}

// entrySize is the memory an entry holds, capacity included. Entries are
// clipped on the way in, so this equals their length -- it is written against
// capacity so that stops being true loudly rather than silently.
func entrySize(data []byte) int64 {
	return int64(cap(data))
}

func (c *Cache) evictOldest() {
	back := c.order.Back()
	if back == nil {
		return
	}
	e := back.Value.(*cacheEntry)
	c.order.Remove(back)
	delete(c.items, e.rid)
	c.curSize -= entrySize(e.data)
}

// Invalidate removes a single rid from the cache.
func (c *Cache) Invalidate(rid libfossil.FslID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[rid]; ok {
		e := elem.Value.(*cacheEntry)
		c.curSize -= entrySize(e.data)
		c.order.Remove(elem)
		delete(c.items, rid)
	}
}

// Clear removes all entries from the cache.
func (c *Cache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[libfossil.FslID]*list.Element)
	c.order.Init()
	c.curSize = 0
}

// Stats returns a snapshot of cache statistics.
func (c *Cache) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Hits:    c.hits,
		Misses:  c.misses,
		Size:    c.curSize,
		MaxSize: c.maxSize,
		Entries: c.order.Len(),
	}
}
