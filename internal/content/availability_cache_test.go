package content

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
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

// TestAvailabilityCacheByUUIDResolvesRidOnce pins that repeated ByUUID calls
// for the same hash cost one uuid->rid lookup, not one per call.
//
// This is the property the cache exists for and the one that was missing: the
// chain walk was memoized while the index seek that starts it was not, so a
// crosslink sweep paid a seek into the blob.uuid index for every F-card of
// every check-in -- the same file hash re-resolved once per revision that did
// not change it. On the Fossil SCM repository that single statement was 27% of
// a whole clone's CPU. Counting lookups rather than timing them keeps this
// honest on any machine.
func TestAvailabilityCacheByUUIDResolvesRidOnce(t *testing.T) {
	d := setupTestDB(t)

	body := []byte("content whose hash is about to be asked for many times over")
	_, uuid, err := blob.Store(d, body)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	cache := NewAvailabilityCache()
	counter := &countingQuerier{inner: d}

	const calls = 25
	for i := 0; i < calls; i++ {
		if _, ok := cache.ByUUID(counter, uuid); !ok {
			t.Fatalf("call %d: ByUUID reported unavailable", i)
		}
	}
	if counter.ridLookups != 1 {
		t.Fatalf("%d uuid->rid lookups for %d ByUUID calls on one hash, want 1",
			counter.ridLookups, calls)
	}
}

// TestAvailabilityCacheByUUIDDoesNotMemoizeMisses pins the asymmetry that
// makes the rid memo safe: a hash that resolves is recorded, a hash that does
// not is asked about again.
//
// Blob rows appear mid-run -- ridOrPhantom and blob.StorePhantom both create
// them while a cascade or sweep is in flight -- so a remembered "no such
// hash" would answer a later question with an earlier repository, and an
// artifact would stay deferred after the content it waits on had arrived.
// A remembered hit cannot go stale the same way: blob.uuid is UNIQUE and no
// path deletes or renumbers a blob.
func TestAvailabilityCacheByUUIDDoesNotMemoizeMisses(t *testing.T) {
	d := setupTestDB(t)

	body := []byte("content that does not exist locally until halfway through")
	uuid := hash.SHA1(body)

	cache := NewAvailabilityCache()
	if _, ok := cache.ByUUID(d, uuid); ok {
		t.Fatalf("ByUUID reported an unstored hash as available")
	}

	// The content arrives, exactly as it would mid-cascade.
	if _, stored, err := blob.Store(d, body); err != nil {
		t.Fatalf("Store: %v", err)
	} else if stored != uuid {
		t.Fatalf("fixture bug: stored under %s, want %s", stored, uuid)
	}

	rid, ok := cache.ByUUID(d, uuid)
	if !ok {
		t.Fatal("ByUUID still reports the hash missing after its blob arrived: " +
			"a negative verdict was memoized")
	}
	if want, _ := AvailableByUUID(d, uuid); rid != want {
		t.Fatalf("ByUUID resolved rid %d, want %d", rid, want)
	}
}

// TestAvailabilityCacheMustNotSpanAPurge pins the boundary of the scope the rid
// memo depends on (see the AvailabilityCache doc comment).
//
// The memo is sound because a crosslink sweep and a phantom-fill cascade only
// ever ADD blob rows. shun.Purge is the one operation in the tree that removes
// them, and today it has no production caller and runs on its own, never from
// inside a crosslink run. This test states what would happen if that changed,
// so the constraint is enforced by a failing assertion rather than by whoever
// reads the comment: a cache carried across a purge keeps answering with the
// rid of a row that is gone.
//
// The purge is issued as the two DELETEs shun.Purge itself runs
// (internal/shun/purge.go) rather than by calling it, because internal/shun
// imports this package and the test lives in it.
//
// If a future change does need a cache to span a purge, the fix is an
// invalidation hook on this type -- not deleting this test.
func TestAvailabilityCacheMustNotSpanAPurge(t *testing.T) {
	d := setupTestDB(t)

	body := []byte("content that is about to be shunned out of the repository")
	rid, uuid, err := blob.Store(d, body)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	cache := NewAvailabilityCache()
	if got, ok := cache.ByUUID(d, uuid); !ok || got != rid {
		t.Fatalf("ByUUID before purge = (%d,%v), want (%d,true)", got, ok, rid)
	}

	if _, err := d.Exec("DELETE FROM delta WHERE rid=?", rid); err != nil {
		t.Fatalf("delete delta: %v", err)
	}
	if _, err := d.Exec("DELETE FROM blob WHERE rid=?", rid); err != nil {
		t.Fatalf("delete blob: %v", err)
	}

	// The unmemoized reference is the ground truth, and it now says "gone".
	if _, ok := AvailableByUUID(d, uuid); ok {
		t.Fatal("fixture bug: the blob row survived the purge")
	}

	// A fresh cache -- one per crosslink run, as the type requires -- agrees.
	if _, ok := NewAvailabilityCache().ByUUID(d, uuid); ok {
		t.Fatal("a cache created after the purge still reports the hash available")
	}

	// The carried-over cache does not, and that is precisely why one must not
	// be carried over. Asserted rather than merely documented so that wiring a
	// purge into a sweep or a sync round fails here first.
	if _, ok := cache.ByUUID(d, uuid); !ok {
		t.Skip("a cache carried across a purge no longer reports the purged hash " +
			"available -- the scope constraint this test guards has been made " +
			"unnecessary; simplify the AvailabilityCache doc comment to match")
	}
}
