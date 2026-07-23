package dst

import (
	"testing"
	"time"

	libsync "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// echoUpstream returns an empty response (immediate convergence).
func echoUpstream() *libsync.MockTransport {
	return &libsync.MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			return &xfer.Message{}
		},
	}
}

func TestNewSimulator(t *testing.T) {
	sim, err := New(SimConfig{
		Seed:      42,
		NumLeaves: 3,
		TmpDir:    t.TempDir(),
		Upstream:  echoUpstream(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	if len(sim.LeafIDs()) != 3 {
		t.Fatalf("leaves = %d, want 3", len(sim.LeafIDs()))
	}
	for _, id := range sim.LeafIDs() {
		if sim.Leaf(id) == nil {
			t.Fatalf("Leaf(%s) = nil", id)
		}
		if sim.Leaf(id).Repo() == nil {
			t.Fatalf("Leaf(%s).Repo() = nil", id)
		}
	}
}

func TestSimulatorSingleStep(t *testing.T) {
	sim, err := New(SimConfig{
		Seed:         1,
		NumLeaves:    1,
		PollInterval: 5 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     echoUpstream(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	more, err := sim.Step()
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !more {
		t.Fatal("expected more events")
	}
	if sim.Steps != 1 {
		t.Fatalf("Steps = %d, want 1", sim.Steps)
	}
	if sim.TotalSyncs != 1 {
		t.Fatalf("TotalSyncs = %d, want 1", sim.TotalSyncs)
	}
}

func TestSimulatorRun(t *testing.T) {
	sim, err := New(SimConfig{
		Seed:         99,
		NumLeaves:    2,
		PollInterval: 5 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     echoUpstream(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	if err := sim.Run(10); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sim.Steps != 10 {
		t.Fatalf("Steps = %d, want 10", sim.Steps)
	}
	if sim.TotalSyncs != 10 {
		t.Fatalf("TotalSyncs = %d, want 10", sim.TotalSyncs)
	}
	if sim.TotalErrors != 0 {
		t.Fatalf("TotalErrors = %d, want 0", sim.TotalErrors)
	}
}

func TestSimulatorRunUntil(t *testing.T) {
	sim, err := New(SimConfig{
		Seed:         7,
		NumLeaves:    2,
		PollInterval: 5 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     echoUpstream(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	// Run for 30 virtual seconds. With 2 leaves at 5s intervals
	// (staggered start), expect ~12 sync events.
	deadline := sim.Clock().Now().Add(30 * time.Second)
	if err := sim.RunUntil(deadline); err != nil {
		t.Fatalf("RunUntil: %v", err)
	}

	if sim.TotalSyncs < 10 {
		t.Fatalf("TotalSyncs = %d, expected >= 10", sim.TotalSyncs)
	}
	// Clock should be at or past the deadline.
	if sim.Clock().Now().Before(deadline) {
		t.Fatalf("clock = %v, expected >= %v", sim.Clock().Now(), deadline)
	}
}

func TestSimulatorScheduleSyncNow(t *testing.T) {
	sim, err := New(SimConfig{
		Seed:         123,
		NumLeaves:    1,
		PollInterval: 1 * time.Hour, // very long — timer won't fire
		TmpDir:       t.TempDir(),
		Upstream:     echoUpstream(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	leafID := sim.LeafIDs()[0]

	// The initial timer event is scheduled at some random offset < 1h.
	// Inject a SyncNow at time zero — it should be processed first.
	sim.ScheduleSyncNow(leafID)

	more, err := sim.Step()
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !more {
		t.Fatal("expected more events")
	}
	if sim.TotalSyncs != 1 {
		t.Fatalf("TotalSyncs = %d, want 1", sim.TotalSyncs)
	}
	// Clock should still be at zero (SyncNow was at time 0).
	if !sim.Clock().Now().Equal(time.Unix(0, 0)) {
		t.Fatalf("clock = %v, expected epoch", sim.Clock().Now())
	}
}

func TestSimulatorNetworkPartition(t *testing.T) {
	sim, err := New(SimConfig{
		Seed:         55,
		NumLeaves:    1,
		PollInterval: 5 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     echoUpstream(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	leafID := sim.LeafIDs()[0]

	// Partition the leaf.
	sim.Network().Partition(leafID)

	// Run one step — sync should fail due to partition.
	sim.Step()
	if sim.TotalErrors != 1 {
		t.Fatalf("TotalErrors = %d, want 1 (partitioned)", sim.TotalErrors)
	}

	// Heal and run another step — should succeed.
	sim.Network().Heal(leafID)
	sim.Step()
	if sim.TotalSyncs != 2 {
		t.Fatalf("TotalSyncs = %d, want 2", sim.TotalSyncs)
	}
	if sim.TotalErrors != 1 {
		t.Fatalf("TotalErrors = %d, want 1 (only the partitioned one)", sim.TotalErrors)
	}
}

func TestSimulatorDropRate(t *testing.T) {
	sim, err := New(SimConfig{
		Seed:         77,
		NumLeaves:    1,
		PollInterval: 1 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     echoUpstream(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	// Set 100% drop rate — all syncs should fail.
	sim.Network().SetDropRate(1.0)

	if err := sim.Run(5); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sim.TotalErrors != 5 {
		t.Fatalf("TotalErrors = %d, want 5 (all dropped)", sim.TotalErrors)
	}
}

func TestSimulatorBuggify(t *testing.T) {
	sim, err := New(SimConfig{
		Seed:         42,
		NumLeaves:    2,
		PollInterval: 1 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     echoUpstream(),
		Buggify:      true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sim.Close()

	// Run many steps — buggify should cause some errors or skipped syncs.
	sim.Run(200)

	t.Logf("Buggify run: %d steps, %d syncs, %d errors",
		sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	// With buggify enabled:
	// - Node Tick may skip syncs (5% on timer events) → TotalSyncs < Steps
	// - Storage buggify (3%) may cause sync errors → TotalErrors > 0
	// We can't assert exact numbers (depends on seed), but log them.
	if sim.TotalSyncs == sim.Steps {
		t.Logf("NOTE: no syncs were skipped by buggify (seed-dependent)")
	}
}

func TestSimulatorBuggifyDeterministic(t *testing.T) {
	run := func(seed int64) (int, int) {
		sim, err := New(SimConfig{
			Seed:         seed,
			NumLeaves:    2,
			PollInterval: 1 * time.Second,
			TmpDir:       t.TempDir(),
			Upstream:     echoUpstream(),
			Buggify:      true,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer sim.Close()
		sim.Run(100)
		return sim.TotalSyncs, sim.TotalErrors
	}

	syncs1, errors1 := run(77)
	syncs2, errors2 := run(77)
	if syncs1 != syncs2 || errors1 != errors2 {
		t.Fatalf("buggify non-deterministic: run1=(%d,%d) run2=(%d,%d)",
			syncs1, errors1, syncs2, errors2)
	}
}

func TestSimulatorDeterministic(t *testing.T) {
	run := func(seed int64) (int, int) {
		sim, err := New(SimConfig{
			Seed:         seed,
			NumLeaves:    3,
			PollInterval: 5 * time.Second,
			TmpDir:       t.TempDir(),
			Upstream:     echoUpstream(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer sim.Close()

		sim.Network().SetDropRate(0.3) // 30% drops
		sim.Run(50)
		return sim.TotalSyncs, sim.TotalErrors
	}

	syncs1, errors1 := run(42)
	syncs2, errors2 := run(42)

	if syncs1 != syncs2 || errors1 != errors2 {
		t.Fatalf("non-deterministic: run1=(%d,%d) run2=(%d,%d)",
			syncs1, errors1, syncs2, errors2)
	}

	// Different seed should (very likely) produce different results.
	syncs3, errors3 := run(999)
	if syncs1 == syncs3 && errors1 == errors3 {
		t.Logf("WARNING: different seeds produced same result (unlikely but possible)")
	}
}
