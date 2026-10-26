package content

import (
	"container/list"
	"sync"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// Cache is a concurrency-safe LRU cache for expanded blob content.
// It reduces redundant delta-chain walks by caching the fully-expanded
// result of [Expand], keyed by rid.
//
// A nil *Cache is valid and acts as a passthrough to [Expand].
type Cache struct {
	mu         sync.Mutex
	items      map[libfossil.FslID]*list.Element
	order      *list.List
	curSize    int64
	maxSize    int64
	maxEntries int
	hits       int64
	misses     int64
	// derived holds the few most recent values callers computed from cached
	// content; derivedOrder is its insertion order, for eviction.
	derived      map[any]any
	derivedOrder []any
	derivedHits  int64
	// sized is the byte-bounded LRU memo of cache_sized.go; sizedOrder is its
	// recency order, most recent first, and sizedSize the bytes it charges.
	sized       map[any]*list.Element
	sizedOrder  *list.List
	sizedSize   int64
	sizedHits   int64
	sizedStores int64
	// unchecked marks a cache made by NewUncheckedCache, which holds bytes
	// not checked against their names and serves only ExpandUnchecked.
	unchecked bool
}

type cacheEntry struct {
	rid  libfossil.FslID
	data []byte
}

// CacheStats reports cache hit/miss statistics and current memory usage.
type CacheStats struct {
	Hits   int64
	Misses int64
	// DerivedHits counts Derived lookups that found a value: reads that
	// reused work done on cached content rather than expanding it again.
	DerivedHits int64
	// SizedHits counts DerivedSized lookups that found a value, and
	// SizedStores the values SetDerivedSized added under a key not held:
	// for the sub-manifest walk, the nodes it reused and the nodes it parsed.
	SizedHits   int64
	SizedStores int64
	Size        int64
	MaxSize     int64
	Entries     int
}

// maxCacheEntries is a fixed hard bound on cache metadata. It is deliberately
// independent of maxSize: content byte accounting remains exact, while empty
// or tiny entries cannot grow the map and LRU list without bound. The limit
// exceeds the 76,700-blob corpus clone and ordinary cache use, so it does not
// perturb their LRU behavior.
const maxCacheEntries = 100_000

// NewCache creates a cache bounded by maxBytes of expanded content and by
// maxCacheEntries metadata entries.
func NewCache(maxBytes int64) *Cache {
	if maxBytes <= 0 {
		panic("content.NewCache: maxBytes must be > 0")
	}
	return &Cache{
		items:      make(map[libfossil.FslID]*list.Element),
		order:      list.New(),
		maxSize:    maxBytes,
		maxEntries: maxCacheEntries,
	}
}

// NewUncheckedCache creates a cache for [Cache.ExpandUnchecked], bounded as
// NewCache bounds one. What it holds is what each row rebuilds to, unchecked
// against the row's name, so it serves no [Cache.Expand].
func NewUncheckedCache(maxBytes int64) *Cache {
	c := NewCache(maxBytes)
	c.unchecked = true
	return c
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
	if c.unchecked {
		panic("content.Cache.Expand: an unchecked cache serves only ExpandUnchecked")
	}
	return c.expand(q, rid, true)
}

// ExpandUnchecked is [ExpandUnchecked] through an unchecked cache: rid's
// bytes, rebuilt from the deepest node of its chain the cache holds, with no
// node checked against its name, and every node it rebuilds kept. Like
// ExpandUnchecked it is for a caller that checks what it derives from the
// bytes, and must not be used to serve content.
func (c *Cache) ExpandUnchecked(q db.Querier, rid libfossil.FslID) ([]byte, error) {
	if c == nil || !c.unchecked {
		panic("content.Cache.ExpandUnchecked: needs a cache from NewUncheckedCache")
	}
	if q == nil {
		panic("content.Cache.ExpandUnchecked: q must not be nil")
	}
	if rid <= 0 {
		panic("content.Cache.ExpandUnchecked: rid must be > 0")
	}
	return c.expand(q, rid, false)
}

// expand serves rid from the cache, or rebuilds it through expandChain with
// check as given, keeping every node it rebuilds.
func (c *Cache) expand(q db.Querier, rid libfossil.FslID, check bool) ([]byte, error) {
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

	data, err := expandChain(q, rid, have, c.store, check)
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
	c.storeOwned(rid, data)
}

// Remember retains a copy of already-expanded content. It is for receive paths
// that have just hash-verified full bytes and must examine those supplied bytes
// before permitting normal cache eviction; callers retain ownership of data.
func (c *Cache) Remember(rid libfossil.FslID, data []byte) {
	if c == nil {
		return
	}
	if rid <= 0 {
		panic("content.Cache.Remember: rid must be > 0")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[rid]; ok {
		c.order.MoveToFront(elem)
		return
	}
	if int64(len(data)) > c.maxSize {
		return
	}

	copied := make([]byte, len(data))
	copy(copied, data)
	c.storeOwnedLocked(rid, copied)
}

func (c *Cache) storeOwned(rid libfossil.FslID, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeOwnedLocked(rid, data)
}

// storeOwnedLocked takes ownership of data as the cached content for rid.
// c.mu must be held.
func (c *Cache) storeOwnedLocked(rid libfossil.FslID, data []byte) {
	if elem, ok := c.items[rid]; ok {
		c.order.MoveToFront(elem)
		return
	}

	data = clip(data)
	elem := c.order.PushFront(&cacheEntry{rid: rid, data: data})
	c.items[rid] = elem
	c.curSize += entrySize(data)

	for (c.curSize > c.maxSize || c.order.Len() > c.maxEntries) && c.order.Len() > 0 {
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

// maxDerived bounds the derived-value memo. A sweep reads each check-in
// beside its parent and baseline, so a handful of recent values is the whole
// working set; the values (parsed manifests) are large, so the bound is small.
const maxDerived = 32

// Derived returns the value SetDerived last stored under key, if it is still
// held. It lets a caller that parses or indexes cached content keep the result
// next to the bytes rather than redo the work for each neighbour that reads
// the same artifact. The value is shared: callers must treat it as read-only.
// A nil Cache holds nothing.
func (c *Cache) Derived(key any) (any, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.derived[key]
	if ok {
		c.derivedHits++
	}
	return v, ok
}

// SetDerived stores v under key, evicting the oldest value past maxDerived.
// key must be comparable. Because Invalidate and Clear drop every derived
// value, v may be computed from content that is immutable only while cached.
func (c *Cache) SetDerived(key, v any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.derived == nil {
		c.derived = make(map[any]any, maxDerived)
	}
	if _, held := c.derived[key]; !held {
		if len(c.derivedOrder) == maxDerived {
			delete(c.derived, c.derivedOrder[0])
			c.derivedOrder = append(c.derivedOrder[:0], c.derivedOrder[1:]...)
		}
		c.derivedOrder = append(c.derivedOrder, key)
	}
	c.derived[key] = v
}

func (c *Cache) dropDerivedLocked() {
	c.derived = nil
	c.derivedOrder = c.derivedOrder[:0]
	c.dropSizedLocked()
}

// Holds reports whether rid's content is cached, without counting a hit or a
// miss or moving it in the eviction order. A nil Cache holds nothing.
func (c *Cache) Holds(rid libfossil.FslID) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[rid]
	return ok
}

// Invalidate removes a single rid from the cache.
func (c *Cache) Invalidate(rid libfossil.FslID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropDerivedLocked()
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
	c.dropDerivedLocked()
}

// Stats returns a snapshot of cache statistics.
func (c *Cache) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Hits:        c.hits,
		Misses:      c.misses,
		DerivedHits: c.derivedHits,
		SizedHits:   c.sizedHits,
		SizedStores: c.sizedStores,
		Size:        c.curSize,
		MaxSize:     c.maxSize,
		Entries:     c.order.Len(),
	}
}
