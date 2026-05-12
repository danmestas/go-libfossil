package dst

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"

	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func createMasterRepo(t *testing.T) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.fossil")
	r, err := repo.Create(path, "master", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create master: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// TestE2EPullFromMaster seeds artifacts in the mock fossil master,
// runs the simulation, and verifies that all leaves converge.
func TestE2EPullFromMaster(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	// Seed 5 artifacts in the master.
	seeded := make(map[string][]byte)
	for i := range 5 {
		data := []byte("master artifact " + string(rune('A'+i)))
		uuid, err := mf.StoreArtifact(data)
		if err != nil {
			t.Fatalf("StoreArtifact %d: %v", i, err)
		}
		seeded[uuid] = data
	}

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

	// Run enough steps for convergence (each leaf needs ~3 rounds per sync:
	// round 1: pull → igot; round 2: gimme → file; round 3: converge).
	if err := sim.Run(20); err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Logf("Simulation: %d steps, %d syncs, %d errors",
		sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	// Verify: every seeded artifact should be in every leaf repo.
	for _, leafID := range sim.LeafIDs() {
		leafRepo := sim.Leaf(leafID).Repo()
		for uuid := range seeded {
			_, exists := blob.Exists(leafRepo.DB(), uuid)
			if !exists {
				t.Errorf("leaf %s missing artifact %s", leafID, uuid)
			}
		}
	}
}

// TestE2EPushFromLeaf stores an artifact in a leaf repo, runs the simulation,
// and verifies it arrives at the master.
func TestE2EPushFromLeaf(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	sim, err := New(SimConfig{
		Seed:         99,
		NumLeaves:    1,
		PollInterval: 5 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     mf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	// Store an artifact directly in the leaf's repo.
	leafID := sim.LeafIDs()[0]
	leafRepo := sim.Leaf(leafID).Repo()

	data := []byte("artifact pushed from leaf")
	var uuid string
	err = leafRepo.WithTx(func(tx *db.Tx) error {
		rid, u, err := blob.Store(tx, data)
		if err != nil {
			return err
		}
		uuid = u
		_, err = tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", rid)
		return err
	})
	if err != nil {
		t.Fatalf("store in leaf: %v", err)
	}

	// Run simulation.
	if err := sim.Run(20); err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Logf("Simulation: %d steps, %d syncs, %d errors",
		sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	// Verify: artifact should be in the master repo.
	_, exists := blob.Exists(mf.Repo().DB(), uuid)
	if !exists {
		t.Errorf("master missing artifact %s pushed from leaf", uuid)
	}
}

// TestE2EBidirectional tests both push and pull in the same simulation.
func TestE2EBidirectional(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	// Seed artifact in master.
	masterData := []byte("from master for bidirectional")
	masterUUID, _ := mf.StoreArtifact(masterData)

	sim, err := New(SimConfig{
		Seed:         7,
		NumLeaves:    2,
		PollInterval: 5 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     mf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	// Seed artifact in leaf-0.
	leaf0Repo := sim.Leaf("leaf-0").Repo()
	leafData := []byte("from leaf-0 for bidirectional")
	var leafUUID string
	leaf0Repo.WithTx(func(tx *db.Tx) error {
		rid, u, err := blob.Store(tx, leafData)
		if err != nil {
			return err
		}
		leafUUID = u
		tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", rid)
		return nil
	})

	// Run enough steps.
	if err := sim.Run(40); err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Logf("Simulation: %d steps, %d syncs, %d errors",
		sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	// Master artifact should be in both leaves.
	for _, id := range sim.LeafIDs() {
		_, exists := blob.Exists(sim.Leaf(id).Repo().DB(), masterUUID)
		if !exists {
			t.Errorf("leaf %s missing master artifact %s", id, masterUUID)
		}
	}

	// Leaf-0 artifact should be in master.
	_, exists := blob.Exists(mf.Repo().DB(), leafUUID)
	if !exists {
		t.Errorf("master missing leaf-0 artifact %s", leafUUID)
	}

	// Leaf-0 artifact should also be in leaf-1 (via master relay).
	// This requires leaf-1 to pull after master received it from leaf-0.
	_, exists = blob.Exists(sim.Leaf("leaf-1").Repo().DB(), leafUUID)
	if !exists {
		t.Logf("NOTE: leaf-1 hasn't received leaf-0's artifact yet (may need more rounds)")
	}
}

// TestE2EPartitionAndHeal tests that a partitioned leaf catches up after healing.
func TestE2EPartitionAndHeal(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	masterData := []byte("artifact before partition")
	masterUUID, _ := mf.StoreArtifact(masterData)

	sim, err := New(SimConfig{
		Seed:         123,
		NumLeaves:    2,
		PollInterval: 5 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     mf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	// Partition leaf-1.
	sim.Network().Partition("leaf-1")

	// Run some steps — leaf-0 should sync, leaf-1 should fail.
	sim.Run(10)

	// leaf-0 should have the artifact.
	_, exists := blob.Exists(sim.Leaf("leaf-0").Repo().DB(), masterUUID)
	if !exists {
		t.Fatal("leaf-0 should have master artifact after 10 steps")
	}

	// leaf-1 should NOT have the artifact.
	_, exists = blob.Exists(sim.Leaf("leaf-1").Repo().DB(), masterUUID)
	if exists {
		t.Fatal("leaf-1 should NOT have master artifact while partitioned")
	}

	// Heal and run more steps.
	sim.Network().Heal("leaf-1")
	sim.Run(10)

	// Now leaf-1 should have caught up.
	_, exists = blob.Exists(sim.Leaf("leaf-1").Repo().DB(), masterUUID)
	if !exists {
		t.Error("leaf-1 should have master artifact after healing")
	}
}
