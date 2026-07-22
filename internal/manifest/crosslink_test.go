package manifest

import (
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestCrosslinkCluster_RemovesFromUnclustered(t *testing.T) {
	r := setupTestRepo(t)
	d := r.DB()

	// Store 5 blobs — each goes into unclustered automatically.
	uuids := make([]string, 5)
	for i := 0; i < 5; i++ {
		_, uuid, err := blob.Store(d, []byte(fmt.Sprintf("blob content %d with enough data", i)))
		if err != nil {
			t.Fatalf("Store blob %d: %v", i, err)
		}
		uuids[i] = uuid
	}

	// Verify all 5 are unclustered.
	var uncBefore int
	d.QueryRow("SELECT count(*) FROM unclustered").Scan(&uncBefore)
	if uncBefore < 5 {
		t.Fatalf("unclustered before = %d, want >= 5", uncBefore)
	}

	// Build and store a cluster artifact referencing those 5 UUIDs.
	clusterDeck := &deck.Deck{Type: deck.Cluster, M: uuids}
	clusterBytes, err := clusterDeck.Marshal()
	if err != nil {
		t.Fatalf("Marshal cluster: %v", err)
	}
	clusterRID, _, err := blob.Store(d, clusterBytes)
	if err != nil {
		t.Fatalf("Store cluster: %v", err)
	}

	// Crosslink the cluster.
	if err := CrosslinkCluster(d, clusterRID, clusterDeck); err != nil {
		t.Fatalf("CrosslinkCluster: %v", err)
	}

	// After crosslink: only the cluster blob itself should remain in unclustered.
	// The 5 referenced blobs should have been removed.
	var uncAfter int
	d.QueryRow("SELECT count(*) FROM unclustered").Scan(&uncAfter)
	// The cluster blob itself is unclustered (it was just stored).
	// The 5 referenced blobs should be removed.
	if uncAfter != 1 {
		t.Fatalf("unclustered after = %d, want 1 (only cluster blob)", uncAfter)
	}
}

func TestCrosslinkCluster_CreatesPhantoms(t *testing.T) {
	r := setupTestRepo(t)
	d := r.DB()

	// Build a cluster referencing UUIDs that don't exist.
	unknownUUIDs := []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	clusterDeck := &deck.Deck{Type: deck.Cluster, M: unknownUUIDs}
	clusterBytes, err := clusterDeck.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	clusterRID, _, err := blob.Store(d, clusterBytes)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	if err := CrosslinkCluster(d, clusterRID, clusterDeck); err != nil {
		t.Fatalf("CrosslinkCluster: %v", err)
	}

	// Verify phantoms were created.
	for _, uuid := range unknownUUIDs {
		var size int64
		err := d.QueryRow("SELECT size FROM blob WHERE uuid=?", uuid).Scan(&size)
		if err != nil {
			t.Fatalf("phantom %s not found: %v", uuid, err)
		}
		if size != -1 {
			t.Fatalf("phantom %s size = %d, want -1", uuid, size)
		}
	}

	// Verify they're in the phantom table.
	var phantomCount int
	d.QueryRow("SELECT count(*) FROM phantom").Scan(&phantomCount)
	if phantomCount != 2 {
		t.Fatalf("phantom count = %d, want 2", phantomCount)
	}
}

func TestCrosslinkCluster_TaggedWithCluster(t *testing.T) {
	r := setupTestRepo(t)
	d := r.DB()

	// Store a blob and build a cluster for it.
	_, uuid, err := blob.Store(d, []byte("tagged content with sufficient length"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	clusterDeck := &deck.Deck{Type: deck.Cluster, M: []string{uuid}}
	clusterBytes, err := clusterDeck.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	clusterRID, _, err := blob.Store(d, clusterBytes)
	if err != nil {
		t.Fatalf("Store cluster: %v", err)
	}

	if err := CrosslinkCluster(d, clusterRID, clusterDeck); err != nil {
		t.Fatalf("CrosslinkCluster: %v", err)
	}

	// Verify tagxref has tagid=7 (cluster), tagtype=1 (singleton).
	var tagid, tagtype int
	err = d.QueryRow(
		"SELECT tagid, tagtype FROM tagxref WHERE srcid=? AND tagid=7", clusterRID,
	).Scan(&tagid, &tagtype)
	if err != nil {
		t.Fatalf("tagxref query: %v", err)
	}
	if tagid != 7 {
		t.Fatalf("tagid = %d, want 7", tagid)
	}
	if tagtype != 1 {
		t.Fatalf("tagtype = %d, want 1", tagtype)
	}
}
