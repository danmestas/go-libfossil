package dst

import (
	"container/heap"
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/danmestas/libfossil/internal/repo"
	libsync "github.com/danmestas/libfossil/internal/sync"
	"github.com/danmestas/libfossil/internal/uv"
	"github.com/danmestas/libfossil/simio"
)

// SeededBuggify implements sync.BuggifyChecker with a deterministic PRNG.
type SeededBuggify struct {
	rng *rand.Rand
}

// Check returns true with the given probability, using the seeded PRNG.
func (b *SeededBuggify) Check(_ string, probability float64) bool {
	return b.rng.Float64() < probability
}

// Simulator drives a deterministic simulation of multiple leaf nodes
// syncing through a simulated network to an upstream transport (mock Fossil master).
type Simulator struct {
	Seed       int64
	rng        *rand.Rand
	clock      *simio.SimClock
	network    *SimNetwork
	events     EventQueue
	leaves     map[NodeID]Node
	masterRepo *repo.Repo // optional: set via SetMasterRepo for UV events targeting "master"

	// Config
	pollInterval        time.Duration
	leafIDs             []NodeID // ordered for deterministic iteration
	buggify             bool
	safetyCheckInterval int

	// Stats
	Steps           int
	TotalSyncs      int
	TotalErrors     int
	TotalUVSent     int
	TotalUVRecvd    int
	TotalUVGimmes   int
}

// SimConfig configures a simulation run.
type SimConfig struct {
	Seed                int64
	NumLeaves           int
	PollInterval        time.Duration
	TmpDir              string           // directory for repo files
	Upstream            libsync.Transport // mock Fossil master
	Buggify             bool              // enable BUGGIFY fault injection
	UV                  bool              // sync unversioned files
	Private             bool              // sync private artifacts
	SafetyCheckInterval int               // run CheckSafety() every N steps; 0 = disabled
}

// New creates a Simulator with the given configuration. It creates
// leaf repos, nodes, and the simulated network. All I/O is
// local SQLite — no NATS or HTTP connections are made.
func New(cfg SimConfig) (*Simulator, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.NumLeaves == 0 {
		cfg.NumLeaves = 2
	}

	rng := rand.New(rand.NewSource(cfg.Seed))
	clock := simio.NewSimClock()

	// Create simulated network connected directly to the upstream transport.
	netRng := rand.New(rand.NewSource(rng.Int63()))
	network := NewSimNetwork(netRng, cfg.Upstream)

	var nodeBuggify libsync.BuggifyChecker
	if cfg.Buggify {
		simio.EnableBuggify(rng.Int63())
		// Wire buggify to MockFossil so server-side handler BUGGIFY fires.
		if mf, ok := cfg.Upstream.(*MockFossil); ok {
			mf.SetBuggify(&SeededBuggify{rng: rand.New(rand.NewSource(rng.Int63()))})
		}
		nodeBuggify = &SeededBuggify{rng: rand.New(rand.NewSource(rng.Int63()))}
	}

	s := &Simulator{
		Seed:                cfg.Seed,
		rng:                 rng,
		clock:               clock,
		network:             network,
		leaves:              make(map[NodeID]Node),
		pollInterval:        cfg.PollInterval,
		buggify:             cfg.Buggify,
		safetyCheckInterval: cfg.SafetyCheckInterval,
	}
	heap.Init(&s.events)

	// Create leaf nodes.
	simRand := simio.NewSeededRand(rng.Int63())
	for i := range cfg.NumLeaves {
		id := NodeID(fmt.Sprintf("leaf-%d", i))

		repoPath := filepath.Join(cfg.TmpDir, fmt.Sprintf("%s.fossil", id))
		r, err := repo.Create(repoPath, "simuser", simRand, "")
		if err != nil {
			return nil, fmt.Errorf("dst: create repo for %s: %w", id, err)
		}

		var projCode, srvCode string
		r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projCode)
		r.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&srvCode)

		transport := network.Transport(id)

		node := NewDefaultNode(r, transport, projCode, srvCode, DefaultNodeOpts{
			Push:       true,
			Pull:       true,
			UV:         cfg.UV,
			XTableSync: true,
			Private:    cfg.Private,
			Buggify:    nodeBuggify,
		})

		s.leaves[id] = node
		s.leafIDs = append(s.leafIDs, id)

		// Schedule initial timer event with a staggered start.
		offset := time.Duration(rng.Int63n(int64(cfg.PollInterval)))
		s.events.PushEvent(&Event{
			Time:   clock.Now().Add(offset),
			Type:   EvTimer,
			NodeID: id,
		})
	}

	return s, nil
}

// Clock returns the simulator's virtual clock.
func (s *Simulator) Clock() *simio.SimClock {
	return s.clock
}

// Network returns the simulated network for fault injection.
func (s *Simulator) Network() *SimNetwork {
	return s.network
}

// Leaf returns the node for the given node ID.
func (s *Simulator) Leaf(id NodeID) Node {
	return s.leaves[id]
}

// LeafIDs returns the ordered list of leaf node IDs.
func (s *Simulator) LeafIDs() []NodeID {
	return s.leafIDs
}

// SetMasterRepo registers the master repo for UV events targeting "master".
func (s *Simulator) SetMasterRepo(r *repo.Repo) {
	s.masterRepo = r
}

// ScheduleSyncNow injects a SyncNow event for the given leaf at the current time.
func (s *Simulator) ScheduleSyncNow(id NodeID) {
	s.events.PushEvent(&Event{
		Time:   s.clock.Now(),
		Type:   EvSyncNow,
		NodeID: id,
	})
}

// ScheduleUVWrite injects a UV write event for the given node at the specified time.
func (s *Simulator) ScheduleUVWrite(id NodeID, at time.Time, name string, data []byte, mtime int64) {
	s.events.PushEvent(&Event{
		Time:    at,
		Type:    EvUVWrite,
		NodeID:  id,
		UVName:  name,
		UVData:  data,
		UVMTime: mtime,
	})
}

// ScheduleUVDelete injects a UV delete event for the given node at the specified time.
func (s *Simulator) ScheduleUVDelete(id NodeID, at time.Time, name string, mtime int64) {
	s.events.PushEvent(&Event{
		Time:    at,
		Type:    EvUVDelete,
		NodeID:  id,
		UVName:  name,
		UVMTime: mtime,
	})
}

// Step processes the next event in the queue. Returns false if the queue is empty.
func (s *Simulator) Step() (bool, error) {
	if s.events.Len() == 0 {
		return false, nil
	}

	ev := s.events.PopEvent()
	s.clock.AdvanceTo(ev.Time)

	s.Steps++

	// Handle UV mutation events directly (no node involved).
	switch ev.Type {
	case EvUVWrite:
		return s.handleUVWrite(ev)
	case EvUVDelete:
		return s.handleUVDelete(ev)
	}

	leaf, ok := s.leaves[ev.NodeID]
	if !ok {
		return true, fmt.Errorf("dst: unknown node %s", ev.NodeID)
	}

	// Execute the sync cycle via the Node interface.
	ctx := context.Background()
	act := leaf.Tick(ctx, ev.Type)

	if act.Type == ActionSynced {
		s.TotalSyncs++
		if act.Err != nil {
			s.TotalErrors++
		}
		if act.Result != nil {
			s.TotalUVSent += act.Result.UVFilesSent
			s.TotalUVRecvd += act.Result.UVFilesRecvd
			s.TotalUVGimmes += act.Result.UVGimmesSent
		}
	}

	// Re-schedule the timer for this leaf.
	if ev.Type == EvTimer {
		s.events.PushEvent(&Event{
			Time:   s.clock.Now().Add(s.pollInterval),
			Type:   EvTimer,
			NodeID: ev.NodeID,
		})
	}

	// Per-step safety check if configured.
	if s.safetyCheckInterval > 0 && s.Steps%s.safetyCheckInterval == 0 {
		if err := s.CheckSafety(); err != nil {
			return false, fmt.Errorf("step %d: %w", s.Steps, err)
		}
	}

	return true, nil
}

func (s *Simulator) resolveNodeRepo(id NodeID) (*repo.Repo, error) {
	if id == "master" {
		if s.masterRepo == nil {
			return nil, fmt.Errorf("dst: master repo not set (call SetMasterRepo)")
		}
		return s.masterRepo, nil
	}
	leaf, ok := s.leaves[id]
	if !ok {
		return nil, fmt.Errorf("dst: unknown node %s", id)
	}
	return leaf.Repo(), nil
}

func (s *Simulator) handleUVWrite(ev *Event) (bool, error) {
	r, err := s.resolveNodeRepo(ev.NodeID)
	if err != nil {
		return true, fmt.Errorf("UV write %q: %w", ev.UVName, err)
	}
	uv.EnsureSchema(r.DB())
	if err := uv.Write(r.DB(), ev.UVName, ev.UVData, ev.UVMTime); err != nil {
		return false, fmt.Errorf("dst: UV write %q on %s: %w", ev.UVName, ev.NodeID, err)
	}
	return true, nil
}

func (s *Simulator) handleUVDelete(ev *Event) (bool, error) {
	r, err := s.resolveNodeRepo(ev.NodeID)
	if err != nil {
		return true, fmt.Errorf("UV delete %q: %w", ev.UVName, err)
	}
	uv.EnsureSchema(r.DB())
	if err := uv.Delete(r.DB(), ev.UVName, ev.UVMTime); err != nil {
		return false, fmt.Errorf("dst: UV delete %q on %s: %w", ev.UVName, ev.NodeID, err)
	}
	return true, nil
}

// Run processes up to maxSteps events. Returns nil on success or the first
// invariant/error encountered.
func (s *Simulator) Run(maxSteps int) error {
	for i := 0; i < maxSteps; i++ {
		more, err := s.Step()
		if err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		if !more {
			break
		}
	}
	return nil
}

// RunUntil processes events until the clock reaches the given time.
func (s *Simulator) RunUntil(deadline time.Time) error {
	for s.events.Len() > 0 {
		// Peek at next event time without popping.
		next := s.events[0]
		if next.Time.After(deadline) {
			s.clock.AdvanceTo(deadline)
			break
		}
		_, err := s.Step()
		if err != nil {
			return err
		}
	}
	return nil
}

// Close cleans up all leaf node repos and disables buggify.
// Iterates leafIDs (not map) for deterministic error reporting.
func (s *Simulator) Close() error {
	if s.buggify {
		simio.DisableBuggify()
	}
	var firstErr error
	for _, id := range s.leafIDs {
		if err := s.leaves[id].Repo().Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
