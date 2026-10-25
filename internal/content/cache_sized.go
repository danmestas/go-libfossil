package content

import "container/list"

// The sized memo is Derived for many small values rather than a few large
// ones: each value is charged the bytes it was derived from, and the memo
// keeps the most recently used values within a quarter of the cache's byte
// budget. Its caller is the sub-manifest walk (draft-fossil-submanifests-00
// §7.4), whose working set is every distinct node of a tree -- far more than
// maxDerived, and a FIFO of that size would evict each node before the next
// check-in's walk reached it, and the parsed check-ins with it.

// sizedEntry is one value of the sized memo.
type sizedEntry struct {
	key  any
	v    any
	size int64
}

// sizedBudget is the bytes the sized memo may charge: a quarter of maxSize.
func (c *Cache) sizedBudget() int64 {
	return c.maxSize / 4
}

// DerivedSized returns the value SetDerivedSized last stored under key, if it
// is still held, and marks it most recently used. The value is shared: callers
// must treat it as read-only. A nil Cache holds nothing.
func (c *Cache) DerivedSized(key any) (any, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.sized[key]
	if !ok {
		return nil, false
	}
	c.sizedOrder.MoveToFront(elem)
	c.sizedHits++
	return elem.Value.(*sizedEntry).v, true
}

// SetDerivedSized stores v under key, charged size bytes, evicting the least
// recently used values past the budget or past maxCacheEntries. A value over
// the whole budget is not kept. key must be comparable. Invalidate and Clear
// drop every value, as they drop Derived's.
func (c *Cache) SetDerivedSized(key, v any, size int64) {
	if c == nil {
		return
	}
	if size < 0 {
		panic("content.Cache.SetDerivedSized: size must be >= 0")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if size > c.sizedBudget() {
		return
	}
	if c.sized == nil {
		c.sized = make(map[any]*list.Element)
		c.sizedOrder = list.New()
	}
	if elem, held := c.sized[key]; held {
		e := elem.Value.(*sizedEntry)
		c.sizedSize += size - e.size
		e.v, e.size = v, size
		c.sizedOrder.MoveToFront(elem)
	} else {
		c.sized[key] = c.sizedOrder.PushFront(&sizedEntry{key: key, v: v, size: size})
		c.sizedSize += size
		c.sizedStores++
	}
	for c.sizedSize > c.sizedBudget() || c.sizedOrder.Len() > c.maxEntries {
		back := c.sizedOrder.Back()
		e := back.Value.(*sizedEntry)
		c.sizedOrder.Remove(back)
		delete(c.sized, e.key)
		c.sizedSize -= e.size
	}
}

func (c *Cache) dropSizedLocked() {
	c.sized = nil
	c.sizedOrder = nil
	c.sizedSize = 0
}
