package dst

import (
	"context"
	"fmt"
	"math/rand"

	libsync "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// SimNetwork simulates the network between leaf agents and the upstream
// transport (mock Fossil master). It supports message dropping, response
// truncation, and network partitions, controlled by a seeded PRNG for
// deterministic behavior.
//
// In the original EdgeSync DST, messages routed through a Bridge that
// forwarded to the upstream. Since Bridge.HandleRequest was just
// upstream.Exchange, SimNetwork now calls the upstream directly.
type SimNetwork struct {
	rng          *rand.Rand
	upstream     libsync.Transport // mock Fossil master
	dropRate     float64           // probability of dropping a message [0, 1)
	truncateRate float64           // probability of truncating response cards [0, 1)
	partitions   map[NodeID]bool   // partitioned nodes (messages to/from are dropped)
}

// NewSimNetwork creates a simulated network connected to the given upstream transport.
func NewSimNetwork(rng *rand.Rand, upstream libsync.Transport) *SimNetwork {
	return &SimNetwork{
		rng:        rng,
		upstream:   upstream,
		partitions: make(map[NodeID]bool),
	}
}

// SetDropRate sets the probability that any message is dropped entirely.
func (n *SimNetwork) SetDropRate(rate float64) {
	n.dropRate = rate
}

// SetTruncateRate sets the probability that a response is truncated
// (random suffix of cards dropped). Simulates partial delivery.
func (n *SimNetwork) SetTruncateRate(rate float64) {
	n.truncateRate = rate
}

// Partition isolates a node — all messages to/from it are dropped.
func (n *SimNetwork) Partition(id NodeID) {
	n.partitions[id] = true
}

// Heal removes a node from the partition set.
func (n *SimNetwork) Heal(id NodeID) {
	delete(n.partitions, id)
}

// HealAll removes all partitions.
func (n *SimNetwork) HealAll() {
	n.partitions = make(map[NodeID]bool)
}

// Transport returns a sync.Transport for the given node that routes
// messages through this simulated network to the upstream.
func (n *SimNetwork) Transport(nodeID NodeID) *SimTransport {
	return &SimTransport{network: n, nodeID: nodeID}
}

// exchange handles a message from a leaf to the upstream, applying fault injection.
func (n *SimNetwork) exchange(ctx context.Context, nodeID NodeID, req *xfer.Message) (*xfer.Message, error) {
	if n.partitions[nodeID] {
		return nil, fmt.Errorf("simnet: node %s is partitioned", nodeID)
	}
	if n.dropRate > 0 && n.rng.Float64() < n.dropRate {
		return nil, fmt.Errorf("simnet: message from %s dropped", nodeID)
	}
	resp, err := n.upstream.Exchange(ctx, req)
	if err != nil {
		return nil, err
	}
	// Truncate: drop a random suffix of response cards to simulate partial delivery.
	if n.truncateRate > 0 && len(resp.Cards) > 1 && n.rng.Float64() < n.truncateRate {
		keep := 1 + n.rng.Intn(len(resp.Cards)-1) // keep at least 1 card
		resp.Cards = resp.Cards[:keep]
	}
	return resp, nil
}

// SimTransport implements sync.Transport by routing through the SimNetwork.
type SimTransport struct {
	network *SimNetwork
	nodeID  NodeID
}

// Exchange sends a request through the simulated network to the upstream.
func (t *SimTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	return t.network.exchange(ctx, t.nodeID, req)
}
