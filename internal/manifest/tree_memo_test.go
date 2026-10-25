package manifest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// These tests pin draft-fossil-submanifests-00 §7.4: sub-manifests are
// memoized by artifact name, so a walk parses each distinct node once and a
// check-in that shares all but one leaf with the last one listed parses only
// its manifest and that leaf.

// memoHistory is two tree check-ins over an interior node and two leaves,
// differing only in their last leaf, and the sub-manifests they reference.
type memoHistory struct {
	f      *treeFixture
	trees  []*deck.Deck
	unique int // distinct sub-manifests across trees
}

func newMemoHistory(t *testing.T) *memoHistory {
	t.Helper()
	f := newTreeFixture(t)
	inner := f.interior(f.leaf(f.file("a.txt")), f.leaf(f.file("b.txt")))
	c := f.leaf(f.file("c.txt"))
	d1 := f.leaf(f.file("d.txt"))
	d2 := f.leaf(deck.FileCard{Name: "d.txt", UUID: f.storeBytes([]byte("d, revised\n"))})
	first := treeDeck(treeTime, []deck.SubmanifestCard{inner, c, d1})
	second := treeDeck(treeTime.Add(time.Minute), []deck.SubmanifestCard{inner, c, d2})
	return &memoHistory{f: f, trees: []*deck.Deck{first, second}, unique: 6}
}

// expansions counts the artifacts cache expanded, each of which the reader
// then parses: a check-in's manifest, or a sub-manifest its walk resolved.
func expansions(cache *content.Cache) int64 {
	st := cache.Stats()
	return st.Hits + st.Misses
}

func TestTreeListingParsesOnlyChangedNodes(t *testing.T) {
	h := newMemoHistory(t)
	rid1, _ := h.f.checkin(h.trees[0])
	rid2, _ := h.f.checkin(h.trees[1])
	cache := content.NewCache(crosslinkCacheBytes)
	assertSameListing(t, h.f, rid1, cache)
	before := expansions(cache)
	assertSameListing(t, h.f, rid2, cache)
	// The manifest and the one leaf it does not share with the first tree.
	if got := expansions(cache) - before; got != 2 {
		t.Fatalf("listing the second tree expanded %d artifacts, want 2", got)
	}
}

// A sweep over a tree history parses each distinct sub-manifest once,
// however many check-ins and readings of each check-in reach it.
func TestTreeCrosslinkParsesEachNodeOnce(t *testing.T) {
	h := newMemoHistory(t)
	for _, d := range h.trees {
		h.f.storeBytes(h.f.marshal(d))
	}
	st := newLinkState(crosslinkCacheBytes)
	linked, err := crosslinkSweepWithState(context.Background(), h.f.r, st)
	if err != nil || linked != len(h.trees) {
		t.Fatalf("sweep linked %d, err %v; want %d", linked, err, len(h.trees))
	}
	stats := st.cache.Stats()
	if stats.SizedStores != int64(h.unique) || stats.SizedHits == 0 {
		t.Fatalf("walks parsed %d sub-manifests and reused %d; want %d parsed, some reused",
			stats.SizedStores, stats.SizedHits, h.unique)
	}
}

// A sub-manifest that is a phantom fails the walk as it does uncached, beside
// nodes the memo holds, and is not remembered as absent: once it arrives the
// same cache lists the tree.
func TestTreeMemoPhantomNode(t *testing.T) {
	h := newMemoHistory(t)
	rid1, _ := h.f.checkin(h.trees[0])
	absentBytes := h.f.marshal(&deck.Deck{F: []deck.FileCard{h.f.file("e.txt")}})
	absent := deck.SubmanifestCard{Name: "e.txt", UUID: repoHash(h.f.r, absentBytes)}
	refs := append(slices.Clone(h.trees[0].S), absent)
	rid3, _ := h.f.checkin(treeDeck(treeTime.Add(2*time.Minute), refs))

	cache := content.NewCache(crosslinkCacheBytes)
	assertSameListing(t, h.f, rid1, cache)
	_, cachedErr := ListFilesCached(h.f.r, rid3, cache)
	_, plainErr := ListFiles(h.f.r, rid3)
	for _, err := range []error{cachedErr, plainErr} {
		if got, ok := MissingSubmanifest(err); !ok || got != absent.UUID {
			t.Fatalf("listing over a phantom node: %v, want the missing %s", err, absent.UUID)
		}
	}
	h.f.storeBytes(absentBytes)
	assertSameListing(t, h.f, rid3, cache)
}

// assertSameListing lists rid through cache and uncached and requires the
// same entries from both.
func assertSameListing(t *testing.T, f *treeFixture, rid libfossil.FslID, cache *content.Cache) {
	t.Helper()
	cached, err := ListFilesCached(f.r, rid, cache)
	if err != nil {
		t.Fatalf("ListFilesCached: %v", err)
	}
	plain, err := ListFiles(f.r, rid)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if !slices.Equal(cached, plain) {
		t.Fatalf("cached listing %v, uncached %v", cached, plain)
	}
}
