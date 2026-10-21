package simio

import (
	mrand "math/rand"
	"sync"
)

// buggifyState holds the global simulation fault injection state.
// Guarded by a mutex for safety, though DST is single-threaded.
var buggifyState struct {
	mu      sync.Mutex
	enabled bool
	rng     *mrand.Rand
}

// EnableBuggify activates fault injection with the given seed.
// Call this at the start of a simulation run.
func EnableBuggify(seed int64) {
	buggifyState.mu.Lock()
	defer buggifyState.mu.Unlock()
	buggifyState.enabled = true
	buggifyState.rng = mrand.New(mrand.NewSource(seed))
}

// DisableBuggify deactivates all fault injection.
// Call this at the end of a simulation run.
func DisableBuggify() {
	buggifyState.mu.Lock()
	defer buggifyState.mu.Unlock()
	buggifyState.enabled = false
	buggifyState.rng = nil
}

// Buggify returns true with the given probability, but ONLY when
// simulation mode is active. In production (not enabled), it always
// returns false with near-zero overhead (single boolean check).
func Buggify(probability float64) bool {
	if !buggifyState.enabled {
		return false
	}
	buggifyState.mu.Lock()
	defer buggifyState.mu.Unlock()
	if !buggifyState.enabled {
		return false
	}
	return buggifyState.rng.Float64() < probability
}

// BuggifyEnabled returns whether simulation fault injection is active.
func BuggifyEnabled() bool {
	return buggifyState.enabled
}

// SuspendBuggify switches fault injection off until the returned resume is
// called, which restores the previous state with its random stream intact.
// An invariant check uses it so it observes what is persisted, not faults
// injected into its own reads, without perturbing the run's later choices.
// Suspensions nest.
func SuspendBuggify() (resume func()) {
	buggifyState.mu.Lock()
	defer buggifyState.mu.Unlock()
	was := buggifyState.enabled
	buggifyState.enabled = false
	return func() {
		buggifyState.mu.Lock()
		defer buggifyState.mu.Unlock()
		buggifyState.enabled = was
	}
}
