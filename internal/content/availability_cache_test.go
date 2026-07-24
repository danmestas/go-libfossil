package content

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestAvailabilityCacheMatchesIsAvailable pins that the memoizing availability
// cache returns, for every kind of blob, exactly what the unmemoized
// IsAvailable/AvailableByUUID pair returns. Methodology: build one grounded
// delta chain, one full-text blob, and one phantom, then assert each verdict
// twice through the cache (second call served from the memo) equals the direct
// answer.
func TestAvailabilityCacheMatchesIsAvailable(t *testing.T) {
	d := setupTestDB(t)

	// Grounded chain: full-text root + one delta on it.
	source := []byte("the original source content for availability cache testing here")
	target := []byte("the original source content for MODIFIED availability cache here")
	srcRid, srcUUID, err := blob.Store(d, source)
	if err != nil {
		t.Fatalf("Store source: %v", err)
	}
	deltaRid, deltaUUID, err := blob.StoreDelta(d, target, srcRid)
	if err != nil {
		t.Fatalf("StoreDelta: %v", err)
	}

	// A phantom blob: present row, unreadable content.
	phantomUUID := "0000000000000000000000000000000000000abc"
	if _, err := blob.StorePhantom(d, phantomUUID); err != nil {
		t.Fatalf("StorePhantom: %v", err)
	}

	cache := NewAvailabilityCache()

	cases := []struct {
		name string
		uuid string
		want bool
	}{
		{"full-text root", srcUUID, true},
		{"grounded delta", deltaUUID, true},
		{"phantom", phantomUUID, false},
		{"unknown uuid", "ffffffffffffffffffffffffffffffffffffffff", false},
	}
	for _, tc := range cases {
		wantRid, want := AvailableByUUID(d, tc.uuid)
		if want != tc.want {
			t.Fatalf("%s: AvailableByUUID reference = %v, want %v", tc.name, want, tc.want)
		}
		// Twice: first computes and memoizes, second is served from the memo.
		for pass := 0; pass < 2; pass++ {
			gotRid, got := cache.ByUUID(d, tc.uuid)
			if got != want || gotRid != wantRid {
				t.Fatalf("%s pass %d: cache.ByUUID = (%d,%v), want (%d,%v)",
					tc.name, pass, gotRid, got, wantRid, want)
			}
		}
	}

	_ = deltaRid
}

// TestAvailabilityCacheServesChainFromMemo proves the cache short-circuits a
// walk at the deepest already-decided ancestor. Methodology: expand a chain one
// node at a time from the root down; after each node's verdict is cached,
// deciding the next (deeper) node must touch only the one new row, which we
// verify by counting queries through a wrapping Querier.
func TestAvailabilityCacheServesChainFromMemo(t *testing.T) {
	d := setupTestDB(t)

	// Build a linear grounded chain root <- d1 <- d2 <- d3, each a delta on the
	// previous. Every node is available.
	root := []byte("chain root content long enough for the delta encoder to bite")
	rootRid, _, err := blob.Store(d, root)
	if err != nil {
		t.Fatalf("Store root: %v", err)
	}
	prev := rootRid
	chain := []libfossil.FslID{rootRid}
	for i := 0; i < 3; i++ {
		next := append([]byte{byte('a' + i)}, root...)
		rid, _, err := blob.StoreDelta(d, next, prev)
		if err != nil {
			t.Fatalf("StoreDelta %d: %v", i, err)
		}
		chain = append(chain, rid)
		prev = rid
	}

	cache := NewAvailabilityCache()
	counter := &countingQuerier{inner: d}

	// Decide root first: walks exactly one node.
	if !cache.isAvailable(counter, rootRid) {
		t.Fatalf("root not available")
	}

	// Deciding each deeper node with its base already memoized must read only
	// the node's own row (one availability step, then a memo hit on its base).
	// Decide nodes shallow-to-deep so each finds its base cached.
	for i := 1; i < len(chain); i++ {
		counter.availSteps = 0
		if !cache.isAvailable(counter, chain[i]) {
			t.Fatalf("chain[%d] not available", i)
		}
		if counter.availSteps != 1 {
			t.Fatalf("chain[%d]: %d availability steps, want 1 (base must be a memo hit)",
				i, counter.availSteps)
		}
	}
}
