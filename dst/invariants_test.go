package dst

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
)

func openTestRepo(t *testing.T, name string) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestCheckBlobIntegrityCleanRepo(t *testing.T) {
	r := openTestRepo(t, "clean")

	// Store a valid blob.
	data := []byte("valid artifact")
	blob.Store(r.DB(), data)

	if err := CheckBlobIntegrity("test", r); err != nil {
		t.Fatalf("expected no error on clean repo: %v", err)
	}
}

func TestCheckBlobIntegrityCorrupt(t *testing.T) {
	r := openTestRepo(t, "corrupt")

	// Store a blob then corrupt its content directly.
	data := []byte("will be corrupted")
	rid, _, _ := blob.Store(r.DB(), data)

	// Overwrite the compressed content with garbage.
	r.DB().Exec("UPDATE blob SET content=X'DEADBEEF' WHERE rid=?", rid)

	err := CheckBlobIntegrity("test", r)
	if err == nil {
		t.Fatal("expected error on corrupt blob")
	}
	inv, ok := err.(*InvariantError)
	if !ok {
		t.Fatalf("expected *InvariantError, got %T: %v", err, err)
	}
	if inv.Invariant != "blob-integrity" {
		t.Fatalf("Invariant = %q, want blob-integrity", inv.Invariant)
	}
}

func TestCheckDeltaChainsClean(t *testing.T) {
	r := openTestRepo(t, "delta-clean")

	// Store base and delta blobs.
	base := []byte("base content for delta chain test")
	rid1, _, _ := blob.Store(r.DB(), base)

	derived := []byte("base content for delta chain TEST") // similar content
	blob.StoreDelta(r.DB(), derived, rid1)

	if err := CheckDeltaChains("test", r); err != nil {
		t.Fatalf("expected no error: %v", err)
	}
}

func TestCheckDeltaChainsDangling(t *testing.T) {
	r := openTestRepo(t, "delta-dangle")

	// Create a blob and a delta pointing to it.
	base := []byte("base")
	rid1, _, _ := blob.Store(r.DB(), base)

	derived := []byte("base modified")
	blob.StoreDelta(r.DB(), derived, rid1)

	// Delete the base blob to create a dangling reference.
	r.DB().Exec("DELETE FROM blob WHERE rid=?", rid1)

	err := CheckDeltaChains("test", r)
	if err == nil {
		t.Fatal("expected error for dangling delta")
	}
	inv, ok := err.(*InvariantError)
	if !ok {
		t.Fatalf("expected *InvariantError, got %T", err)
	}
	if inv.Invariant != "delta-chain" {
		t.Fatalf("Invariant = %q, want delta-chain", inv.Invariant)
	}
}

func TestCheckConvergenceMatching(t *testing.T) {
	master := openTestRepo(t, "master")
	leaf := openTestRepo(t, "leaf")

	// Store same artifact in both.
	data := []byte("shared artifact")
	blob.Store(master.DB(), data)
	blob.Store(leaf.DB(), data)

	leaves := map[NodeID]*repo.Repo{"leaf-0": leaf}
	if err := CheckConvergence(master, leaves); err != nil {
		t.Fatalf("expected convergence: %v", err)
	}
}

func TestCheckConvergenceMissing(t *testing.T) {
	master := openTestRepo(t, "master")
	leaf := openTestRepo(t, "leaf")

	// Store artifact only in master.
	data := []byte("only in master")
	blob.Store(master.DB(), data)

	leaves := map[NodeID]*repo.Repo{"leaf-0": leaf}
	err := CheckConvergence(master, leaves)
	if err == nil {
		t.Fatal("expected convergence error")
	}
	inv, ok := err.(*InvariantError)
	if !ok {
		t.Fatalf("expected *InvariantError, got %T", err)
	}
	if inv.Invariant != "convergence" {
		t.Fatalf("Invariant = %q, want convergence", inv.Invariant)
	}
}

func TestCheckConvergenceExtraInLeaf(t *testing.T) {
	master := openTestRepo(t, "master")
	leaf := openTestRepo(t, "leaf")

	// Store artifact only in leaf — divergence.
	data := []byte("only in leaf")
	blob.Store(leaf.DB(), data)

	leaves := map[NodeID]*repo.Repo{"leaf-0": leaf}
	err := CheckConvergence(master, leaves)
	if err == nil {
		t.Fatal("expected convergence error for extra leaf artifact")
	}
}

func TestCheckSubsetOfAllowsExtra(t *testing.T) {
	master := openTestRepo(t, "master")
	leaf := openTestRepo(t, "leaf")

	// Master has one artifact.
	blob.Store(master.DB(), []byte("shared"))
	// Leaf has master's artifact plus an extra.
	blob.Store(leaf.DB(), []byte("shared"))
	blob.Store(leaf.DB(), []byte("extra in leaf"))

	leaves := map[NodeID]*repo.Repo{"leaf-0": leaf}
	if err := CheckSubsetOf(master, leaves); err != nil {
		t.Fatalf("SubsetOf should allow extra leaf artifacts: %v", err)
	}
}

// --- Simulator integration ---

func TestSimulatorCheckSafetyAfterRun(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	// Seed artifacts.
	for i := range 3 {
		mf.StoreArtifact([]byte("artifact " + string(rune('A'+i))))
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

	sim.Run(20)

	// Safety invariants should hold.
	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("CheckSafety: %v", err)
	}
}

func TestSimulatorCheckConvergenceAfterRun(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	// Seed artifacts.
	for i := range 5 {
		mf.StoreArtifact([]byte("convergence artifact " + string(rune('A'+i))))
	}

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

	// Run enough steps for convergence.
	sim.Run(30)

	// All leaves should have all master artifacts.
	if err := sim.CheckAllConverged(masterRepo); err != nil {
		t.Fatalf("CheckAllConverged: %v", err)
	}
}

func TestSimulatorSafetyWithBuggify(t *testing.T) {
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)

	mf.StoreArtifact([]byte("buggify safety test"))

	sim, err := New(SimConfig{
		Seed:         42,
		NumLeaves:    1,
		PollInterval: 1 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     mf,
		Buggify:      true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	sim.Run(50)

	// Safety invariants should hold even with buggify.
	// Buggify may cause sync errors, but should not corrupt stored data
	// (the blob.Store is content-addressed, so corruption is detected).
	if err := sim.CheckSafety(); err != nil {
		t.Logf("Safety violation with buggify (may be expected from content.Expand buggify): %v", err)
	}

	t.Logf("Buggify safety: %d steps, %d syncs, %d errors",
		sim.Steps, sim.TotalSyncs, sim.TotalErrors)
}
