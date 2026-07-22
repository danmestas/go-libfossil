package dst

import (
	"context"
	"fmt"
	"math/rand"

	"github.com/danmestas/go-libfossil/internal/repo"
	libsync "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// PeerNetwork simulates a peer-to-peer network where each leaf's
// Exchange calls HandleSync on the designated peer's repo. No bridge,
// no central server — pure leaf-to-leaf sync under DST control.
type PeerNetwork struct {
	rng        *rand.Rand
	peers      map[NodeID]*repo.Repo // each leaf's repo
	dropRate   float64
	partitions map[NodeID]bool
	buggify    libsync.BuggifyChecker
}

// NewPeerNetwork creates a simulated peer network.
func NewPeerNetwork(rng *rand.Rand) *PeerNetwork {
	if rng == nil {
		panic("dst.NewPeerNetwork: rng must not be nil")
	}
	return &PeerNetwork{
		rng:        rng,
		peers:      make(map[NodeID]*repo.Repo),
		partitions: make(map[NodeID]bool),
	}
}

// AddPeer registers a leaf's repo as a sync target for other leaves.
func (n *PeerNetwork) AddPeer(id NodeID, r *repo.Repo) {
	if r == nil {
		panic("PeerNetwork.AddPeer: r must not be nil")
	}
	n.peers[id] = r
}

// SetDropRate sets the probability that any message is dropped.
func (n *PeerNetwork) SetDropRate(rate float64) {
	n.dropRate = rate
}

// SetBuggify configures fault injection for the handler.
func (n *PeerNetwork) SetBuggify(b libsync.BuggifyChecker) {
	n.buggify = b
}

// Partition isolates a node — all messages to/from it are dropped.
func (n *PeerNetwork) Partition(id NodeID) {
	n.partitions[id] = true
}

// Heal removes a node from the partition set.
func (n *PeerNetwork) Heal(id NodeID) {
	delete(n.partitions, id)
}

// HealAll removes all partitions.
func (n *PeerNetwork) HealAll() {
	n.partitions = make(map[NodeID]bool)
}

// Transport returns a sync.Transport for the given source node that
// routes to a specific target peer's HandleSync.
func (n *PeerNetwork) Transport(source, target NodeID) *PeerTransport {
	return &PeerTransport{
		network: n,
		source:  source,
		target:  target,
	}
}

// exchange handles a message from source to target, applying fault injection.
func (n *PeerNetwork) exchange(ctx context.Context, source, target NodeID, req *xfer.Message) (*xfer.Message, error) {
	if n.partitions[source] {
		return nil, fmt.Errorf("peernet: source %s is partitioned", source)
	}
	if n.partitions[target] {
		return nil, fmt.Errorf("peernet: target %s is partitioned", target)
	}
	if n.dropRate > 0 && n.rng.Float64() < n.dropRate {
		return nil, fmt.Errorf("peernet: message from %s to %s dropped", source, target)
	}

	r, ok := n.peers[target]
	if !ok {
		return nil, fmt.Errorf("peernet: unknown target %s", target)
	}

	return libsync.HandleSyncWithOpts(ctx, r, req, libsync.HandleOpts{
		Buggify: n.buggify,
	})
}

// PeerTransport implements sync.Transport by routing through the PeerNetwork
// to a specific target peer.
type PeerTransport struct {
	network *PeerNetwork
	source  NodeID
	target  NodeID
}

// Exchange sends a request through the peer network to the target's HandleSync.
func (t *PeerTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	if req == nil {
		panic("PeerTransport.Exchange: req must not be nil")
	}
	return t.network.exchange(ctx, t.source, t.target, req)
}
