package content

import "testing"

func TestCacheDerivedSized(t *testing.T) {
	var nilCache *Cache
	nilCache.SetDerivedSized("k", 1, 1)
	if _, ok := nilCache.DerivedSized("k"); ok {
		t.Fatal("nil cache held a sized value")
	}

	c := NewCache(400) // a sized budget of 100
	if _, ok := c.DerivedSized("k"); ok {
		t.Fatal("empty cache held a sized value")
	}
	c.SetDerivedSized("a", "A", 40)
	c.SetDerivedSized("b", "B", 40)
	if v, ok := c.DerivedSized("a"); !ok || v != "A" {
		t.Fatalf("DerivedSized(a) = %v, %v; want A, true", v, ok)
	}
	// a was read last, so b is the least recently used and goes first.
	c.SetDerivedSized("c", "C", 40)
	if _, ok := c.DerivedSized("b"); ok {
		t.Fatal("the least recently used value survived past the byte budget")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.DerivedSized(k); !ok {
			t.Fatalf("DerivedSized(%s) evicted within the budget", k)
		}
	}
	// A value larger than the whole budget is not kept, and evicts nothing.
	c.SetDerivedSized("huge", "H", 101)
	if _, ok := c.DerivedSized("huge"); ok {
		t.Fatal("a value over the budget was kept")
	}
	if _, ok := c.DerivedSized("a"); !ok {
		t.Fatal("an over-budget value evicted a held one")
	}
	// Re-setting a held key replaces it without counting a store.
	c.SetDerivedSized("a", "A2", 40)
	st := c.Stats()
	if st.SizedStores != 3 || st.SizedHits != 4 {
		t.Fatalf("SizedStores = %d, SizedHits = %d; want 3, 4", st.SizedStores, st.SizedHits)
	}
	if v, _ := c.DerivedSized("a"); v != "A2" {
		t.Fatalf("DerivedSized(a) = %v after re-setting it", v)
	}

	c.Invalidate(1)
	if _, ok := c.DerivedSized("a"); ok {
		t.Fatal("Invalidate kept a sized value")
	}
	c.SetDerivedSized("a", "A", 40)
	c.Clear()
	if _, ok := c.DerivedSized("a"); ok {
		t.Fatal("Clear kept a sized value")
	}
}
