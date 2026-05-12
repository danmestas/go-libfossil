package dst

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/branch"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
)

// TestTagPropagationAcrossSync creates checkins with inline T-cards on the
// master, syncs to leaves, and verifies tagxref entries propagated correctly.
func TestTagPropagationAcrossSync(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	// Create checkins via manifest.Checkin.
	parentRid, _, err := manifest.Checkin(masterRepo, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello world")}},
		Comment: "initial commit with tags",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin initial: %v", err)
	}

	// Second commit — should inherit branch=trunk via tag propagation.
	_, _, err = manifest.Checkin(masterRepo, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello world v2")}},
		Comment: "second commit",
		User:    "testuser",
		Parent:  parentRid,
		Time:    time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin second: %v", err)
	}

	// Verify tagxref integrity on master before sync.
	if err := CheckTagxrefIntegrity("master", masterRepo); err != nil {
		t.Fatalf("master tagxref integrity: %v", err)
	}

	// Run simulation: master -> 2 leaves.
	sim, err := New(SimConfig{
		Seed:         42,
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

	// After sync, run Crosslink on each leaf to process received manifests.
	for _, id := range sim.LeafIDs() {
		leafRepo := sim.Leaf(id).Repo()

		n, err := manifest.Crosslink(leafRepo)
		if err != nil {
			t.Fatalf("Crosslink %s: %v", id, err)
		}
		t.Logf("%s: crosslinked %d artifacts", id, n)

		// Verify tagxref integrity on each leaf.
		if err := CheckTagxrefIntegrity(string(id), leafRepo); err != nil {
			t.Errorf("tagxref integrity %s: %v", id, err)
		}

		// Verify branch=trunk tag exists on at least the initial checkin.
		var branchCount int
		leafRepo.DB().QueryRow(
			"SELECT count(*) FROM tagxref JOIN tag USING(tagid) WHERE tagname='branch' AND value='trunk'",
		).Scan(&branchCount)
		if branchCount == 0 {
			t.Errorf("%s: no branch=trunk tagxref entries after sync", id)
		} else {
			t.Logf("%s: %d branch=trunk tagxref entries", id, branchCount)
		}
	}
}

// TestBranchCreateAndSync creates a branch on the master, syncs to leaves,
// and verifies the branch is visible via branch.List on each leaf.
func TestBranchCreateAndSync(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	// Create initial checkin on trunk.
	parentRid, _, err := manifest.Checkin(masterRepo, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "main.go", Content: []byte("package main")}},
		Comment: "initial commit",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Create a branch.
	_, _, err = branch.Create(masterRepo, branch.CreateOpts{
		Name:   "feature-x",
		Parent: parentRid,
		User:   "testuser",
		Time:   time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("branch.Create: %v", err)
	}

	// Verify branches on master.
	masterBranches, err := branch.List(masterRepo)
	if err != nil {
		t.Fatalf("master branch.List: %v", err)
	}
	masterNames := map[string]bool{}
	for _, b := range masterBranches {
		masterNames[b.Name] = true
		t.Logf("master branch: %s closed=%v", b.Name, b.IsClosed)
	}
	if !masterNames["feature-x"] {
		t.Fatal("master missing branch feature-x")
	}

	// Verify tagxref integrity on master.
	if err := CheckTagxrefIntegrity("master", masterRepo); err != nil {
		t.Fatalf("master tagxref: %v", err)
	}

	// Run simulation: master -> 2 leaves.
	sim, err := New(SimConfig{
		Seed:         77,
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

	// After sync, crosslink + verify branches on each leaf.
	for _, id := range sim.LeafIDs() {
		leafRepo := sim.Leaf(id).Repo()

		n, err := manifest.Crosslink(leafRepo)
		if err != nil {
			t.Fatalf("Crosslink %s: %v", id, err)
		}
		t.Logf("%s: crosslinked %d artifacts", id, n)

		// Check tagxref integrity.
		if err := CheckTagxrefIntegrity(string(id), leafRepo); err != nil {
			t.Errorf("tagxref integrity %s: %v", id, err)
		}

		// Verify branch is visible via branch.List.
		leafBranches, err := branch.List(leafRepo)
		if err != nil {
			t.Errorf("%s branch.List: %v", id, err)
			continue
		}
		leafNames := map[string]bool{}
		for _, b := range leafBranches {
			leafNames[b.Name] = true
			t.Logf("%s branch: %s closed=%v", id, b.Name, b.IsClosed)
		}
		if !leafNames["feature-x"] {
			t.Errorf("%s: missing branch feature-x after sync", id)
		}
	}
}

// TestTagxrefIntegrityInvariant is a unit test for the invariant itself.
func TestTagxrefIntegrityInvariant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer r.Close()

	// Empty repo should pass.
	if err := CheckTagxrefIntegrity("test", r); err != nil {
		t.Fatalf("empty repo: %v", err)
	}

	// Create a checkin (adds inline T-cards which populate tagxref).
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("a")}},
		Comment: "test",
		User:    "testuser",
		Time:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Repo with tags should pass.
	if err := CheckTagxrefIntegrity("test", r); err != nil {
		t.Fatalf("with tags: %v", err)
	}

	// Corrupt: insert tagxref referencing non-existent blob.
	r.DB().Exec("INSERT INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) VALUES(1, 1, 0, 99999, '', 0, 99999)")
	if err := CheckTagxrefIntegrity("test", r); err == nil {
		t.Error("expected invariant violation for non-existent rid")
	}
}
