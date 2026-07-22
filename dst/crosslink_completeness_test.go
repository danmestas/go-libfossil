package dst

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/manifest"
)

// TestCrosslinkCompletenessAfterSync verifies that after syncing checkins from
// master to leaves, running manifest.Crosslink on each leaf correctly populates
// event rows, plink rows, and branch tags. This ensures Crosslink handles all
// artifact types and relationships after a sync.
func TestCrosslinkCompletenessAfterSync(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	// Create several checkins on master with parent relationships.
	rid1, uuid1, err := manifest.Checkin(masterRepo, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file1.txt", Content: []byte("content1")}},
		Comment: "first commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin 1: %v", err)
	}

	rid2, uuid2, err := manifest.Checkin(masterRepo, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file2.txt", Content: []byte("content2")}},
		Comment: "second commit",
		User:    "testuser",
		Parent:  rid1,
		Time:    time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin 2: %v", err)
	}

	_, uuid3, err := manifest.Checkin(masterRepo, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file3.txt", Content: []byte("content3")}},
		Comment: "third commit",
		User:    "testuser",
		Parent:  rid2,
		Time:    time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin 3: %v", err)
	}

	// Verify master has expected metadata before sync.
	var masterEventCount, masterPlinkCount, masterBranchCount int
	masterRepo.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&masterEventCount)
	masterRepo.DB().QueryRow("SELECT count(*) FROM plink").Scan(&masterPlinkCount)
	masterRepo.DB().QueryRow(`
		SELECT count(*) FROM tagxref
		JOIN tag USING(tagid)
		WHERE tagname='branch'
	`).Scan(&masterBranchCount)
	t.Logf("master before sync: %d event rows, %d plink rows, %d branch tags",
		masterEventCount, masterPlinkCount, masterBranchCount)

	if masterEventCount != 3 {
		t.Fatalf("master event count=%d, want 3", masterEventCount)
	}
	if masterPlinkCount != 2 {
		t.Fatalf("master plink count=%d, want 2 (rid1->rid2, rid2->rid3)", masterPlinkCount)
	}
	if masterBranchCount < 1 {
		t.Fatalf("master branch tags=%d, want >= 1", masterBranchCount)
	}

	// Run simulation: master -> 2 leaves.
	sim, err := New(SimConfig{
		Seed:         100,
		NumLeaves:    2,
		PollInterval: 5 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     mf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	if err := sim.Run(30); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("Simulation: %d steps, %d syncs, %d errors", sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	// After sync, run manifest.Crosslink on each leaf and verify metadata.
	for _, id := range sim.LeafIDs() {
		leafRepo := sim.Leaf(id).Repo()

		// Run Crosslink.
		n, err := manifest.Crosslink(leafRepo)
		if err != nil {
			t.Fatalf("Crosslink %s: %v", id, err)
		}
		t.Logf("%s: crosslinked %d artifacts", id, n)

		// Verify event rows > 0 (should have all 3 checkins).
		var eventCount int
		leafRepo.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&eventCount)
		if eventCount == 0 {
			t.Errorf("%s: event count=0, want > 0", id)
		} else {
			t.Logf("%s: %d event rows", id, eventCount)
		}

		// Verify plink rows > 0 (should have parent relationships).
		var plinkCount int
		leafRepo.DB().QueryRow("SELECT count(*) FROM plink").Scan(&plinkCount)
		if plinkCount == 0 {
			t.Errorf("%s: plink count=0, want > 0", id)
		} else {
			t.Logf("%s: %d plink rows", id, plinkCount)
		}

		// Verify branch tags > 0 (should have trunk branch tags).
		var branchCount int
		leafRepo.DB().QueryRow(`
			SELECT count(*) FROM tagxref
			JOIN tag USING(tagid)
			WHERE tagname='branch'
		`).Scan(&branchCount)
		if branchCount == 0 {
			t.Errorf("%s: branch tag count=0, want > 0", id)
		} else {
			t.Logf("%s: %d branch tags", id, branchCount)
		}

		// Verify tagxref integrity.
		if err := CheckTagxrefIntegrity(string(id), leafRepo); err != nil {
			t.Errorf("tagxref integrity %s: %v", id, err)
		}

		// Verify leaf has all 3 checkins from master (by UUID).
		var checkin1, checkin2, checkin3 int
		leafRepo.DB().QueryRow(`
			SELECT count(*) FROM event e JOIN blob b ON e.objid=b.rid WHERE b.uuid=?
		`, uuid1).Scan(&checkin1)
		leafRepo.DB().QueryRow(`
			SELECT count(*) FROM event e JOIN blob b ON e.objid=b.rid WHERE b.uuid=?
		`, uuid2).Scan(&checkin2)
		leafRepo.DB().QueryRow(`
			SELECT count(*) FROM event e JOIN blob b ON e.objid=b.rid WHERE b.uuid=?
		`, uuid3).Scan(&checkin3)
		if checkin1 == 0 || checkin2 == 0 || checkin3 == 0 {
			t.Errorf("%s: missing checkins (uuid1=%d, uuid2=%d, uuid3=%d)",
				id, checkin1, checkin2, checkin3)
		}
	}
}
