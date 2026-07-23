package content

import (
	"fmt"
	"sort"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestGenerateClusters_BelowThreshold(t *testing.T) {
	d := setupTestDB(t)

	// Store 99 blobs (below threshold of 100).
	for i := 0; i < 99; i++ {
		_, _, err := blob.Store(d, []byte(fmt.Sprintf("blob-%04d-content-padding", i)))
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}

	n, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 0 {
		t.Fatalf("clusters = %d, want 0", n)
	}

	// All 99 should still be unclustered.
	var unc int
	d.QueryRow("SELECT count(*) FROM unclustered").Scan(&unc)
	if unc != 99 {
		t.Fatalf("unclustered = %d, want 99", unc)
	}
}

func TestGenerateClusters_AtThreshold(t *testing.T) {
	d := setupTestDB(t)

	for i := 0; i < 100; i++ {
		_, _, err := blob.Store(d, []byte(fmt.Sprintf("blob-%04d-content-padding", i)))
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}

	n, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 1 {
		t.Fatalf("clusters = %d, want 1", n)
	}

	// Only the cluster blob itself should remain unclustered.
	var unc int
	d.QueryRow("SELECT count(*) FROM unclustered").Scan(&unc)
	if unc != 1 {
		t.Fatalf("unclustered = %d, want 1", unc)
	}

	// Verify tagid=7 was applied.
	var tagCount int
	d.QueryRow("SELECT count(*) FROM tagxref WHERE tagid=7 AND tagtype=1").Scan(&tagCount)
	if tagCount != 1 {
		t.Fatalf("cluster tag count = %d, want 1", tagCount)
	}
}

func TestGenerateClusters_MultipleClusters(t *testing.T) {
	d := setupTestDB(t)

	for i := 0; i < 2000; i++ {
		_, _, err := blob.Store(d, []byte(fmt.Sprintf("blob-%05d-content-padding-extra", i)))
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}

	n, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 3 {
		t.Fatalf("clusters = %d, want 3 (800+800+400)", n)
	}

	// Only the 3 cluster blobs should remain unclustered.
	var unc int
	d.QueryRow("SELECT count(*) FROM unclustered").Scan(&unc)
	if unc != 3 {
		t.Fatalf("unclustered = %d, want 3", unc)
	}
}

func TestGenerateClusters_Idempotent(t *testing.T) {
	d := setupTestDB(t)

	for i := 0; i < 200; i++ {
		_, _, err := blob.Store(d, []byte(fmt.Sprintf("blob-%04d-content-padding", i)))
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}

	n1, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("first GenerateClusters: %v", err)
	}
	if n1 != 1 {
		t.Fatalf("first call clusters = %d, want 1", n1)
	}

	// Second call: only 1 unclustered blob (the cluster itself) — below threshold.
	n2, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("second GenerateClusters: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second call clusters = %d, want 0", n2)
	}
}

func TestGenerateClusters_PhantomsExcluded(t *testing.T) {
	d := setupTestDB(t)

	// Store 80 real blobs.
	for i := 0; i < 80; i++ {
		_, _, err := blob.Store(d, []byte(fmt.Sprintf("real-blob-%04d-content-padding", i)))
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}

	// Store 30 phantoms and manually insert into unclustered.
	for i := 0; i < 30; i++ {
		uuid := fmt.Sprintf("%040d", i+10000)
		rid, err := blob.StorePhantom(d, uuid)
		if err != nil {
			t.Fatalf("StorePhantom %d: %v", i, err)
		}
		d.Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rid)
	}

	// Total unclustered = 110, but only 80 are non-phantom → below threshold.
	n, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 0 {
		t.Fatalf("clusters = %d, want 0 (80 real < 100 threshold)", n)
	}
}

func TestGenerateClusters_ShunnedExcluded(t *testing.T) {
	d := setupTestDB(t)

	// Store 100 real blobs (at threshold).
	for i := 0; i < 100; i++ {
		_, uuid, err := blob.Store(d, []byte(fmt.Sprintf("blob-%04d-content-padding", i)))
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
		// Shun the last 10 blobs.
		if i >= 90 {
			if _, err := d.Exec("INSERT INTO shun(uuid, mtime) VALUES(?, 0)", uuid); err != nil {
				t.Fatalf("shun %d: %v", i, err)
			}
		}
	}

	// 90 non-shunned blobs < 100 threshold -> no clusters.
	n, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 0 {
		t.Fatalf("clusters = %d, want 0 (90 non-shunned < 100 threshold)", n)
	}
}

func TestGenerateClusters_PrivateExcluded(t *testing.T) {
	d := setupTestDB(t)

	var rids []libfossil.FslID
	for i := 0; i < 100; i++ {
		rid, _, err := blob.Store(d, []byte(fmt.Sprintf("blob-%04d-content-padding", i)))
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
		rids = append(rids, rid)
	}

	for _, rid := range rids[90:] {
		if _, err := d.Exec("INSERT INTO private(rid) VALUES(?)", rid); err != nil {
			t.Fatalf("private: %v", err)
		}
	}

	n, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 0 {
		t.Fatalf("clusters = %d, want 0 (90 non-private < 100 threshold)", n)
	}
}

func TestGenerateClusters_PrivateStayUnclustered(t *testing.T) {
	d := setupTestDB(t)

	var privateRids []libfossil.FslID
	for i := 0; i < 110; i++ {
		rid, _, err := blob.Store(d, []byte(fmt.Sprintf("blob-%05d-content-padding-extra", i)))
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
		if i >= 100 {
			if _, err := d.Exec("INSERT INTO private(rid) VALUES(?)", rid); err != nil {
				t.Fatalf("private: %v", err)
			}
			privateRids = append(privateRids, rid)
		}
	}

	n, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 1 {
		t.Fatalf("clusters = %d, want 1", n)
	}

	for _, rid := range privateRids {
		var count int
		if err := d.QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid).Scan(&count); err != nil {
			t.Fatalf("query unclustered rid=%d: %v", rid, err)
		}
		if count != 1 {
			t.Fatalf("private rid=%d missing from unclustered", rid)
		}
	}
}

func TestGenerateClusters_ValidArtifactFormat(t *testing.T) {
	d := setupTestDB(t)

	storedUUIDs := make([]string, 100)
	for i := 0; i < 100; i++ {
		_, uuid, err := blob.Store(d, []byte(fmt.Sprintf("blob-%04d-content-padding", i)))
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
		storedUUIDs[i] = uuid
	}

	n, err := GenerateClusters(d)
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 1 {
		t.Fatalf("clusters = %d, want 1", n)
	}

	// Find the cluster blob (tagged with tagid=7).
	var clusterRID int64
	err = d.QueryRow("SELECT srcid FROM tagxref WHERE tagid=7 AND tagtype=1").Scan(&clusterRID)
	if err != nil {
		t.Fatalf("find cluster: %v", err)
	}

	// Expand and parse the cluster artifact.
	data, err := Expand(d, libfossil.FslID(clusterRID))
	if err != nil {
		t.Fatalf("Expand cluster: %v", err)
	}

	parsed, err := deck.Parse(data)
	if err != nil {
		t.Fatalf("Parse cluster: %v", err)
	}

	if parsed.Type != deck.Cluster {
		t.Fatalf("type = %d, want Cluster", parsed.Type)
	}
	if len(parsed.M) != 100 {
		t.Fatalf("M-cards = %d, want 100", len(parsed.M))
	}

	// Verify M-cards are sorted.
	if !sort.StringsAreSorted(parsed.M) {
		t.Fatal("M-cards are not sorted")
	}

	// Verify all stored UUIDs are present.
	sort.Strings(storedUUIDs)
	for i, uuid := range storedUUIDs {
		if parsed.M[i] != uuid {
			t.Fatalf("M[%d] = %s, want %s", i, parsed.M[i], uuid)
		}
	}
}
