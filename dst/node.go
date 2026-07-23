package dst

import (
	"context"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
)

// ActionType classifies the result of processing an event.
type ActionType int

const (
	ActionNone   ActionType = iota
	ActionSynced                   // a sync round was executed
)

// Action is the result of processing a single event via Tick.
type Action struct {
	Type   ActionType
	Result *sync.SyncResult // non-nil when Type == ActionSynced
	Err    error            // non-nil on sync failure
}

// Node represents a sync participant in the simulation.
// The simulator drives events by calling Tick; the Node executes a sync
// cycle and reports the result. EdgeSync can implement this by wrapping
// its agent.Agent; the DST provides a DefaultNode for in-module testing.
type Node interface {
	Tick(ctx context.Context, event EventType) Action
	Repo() *repo.Repo
}

// DefaultNode is a simple Node that calls sync.Sync directly.
// No NATS, no HTTP — just a repo + transport.
type DefaultNode struct {
	repo        *repo.Repo
	transport   sync.Transport
	projectCode string
	serverCode  string
	opts        DefaultNodeOpts
	buggify     sync.BuggifyChecker
}

// DefaultNodeOpts configures a DefaultNode.
type DefaultNodeOpts struct {
	Push       bool
	Pull       bool
	UV         bool
	XTableSync bool
	Private    bool
	Buggify    sync.BuggifyChecker
}

// NewDefaultNode creates a Node backed by sync.Sync.
func NewDefaultNode(r *repo.Repo, t sync.Transport, projectCode, serverCode string, opts DefaultNodeOpts) *DefaultNode {
	// Default to push+pull if neither specified.
	if !opts.Push && !opts.Pull {
		opts.Push = true
		opts.Pull = true
	}
	return &DefaultNode{
		repo:        r,
		transport:   t,
		projectCode: projectCode,
		serverCode:  serverCode,
		opts:        opts,
		buggify:     opts.Buggify,
	}
}

// Tick executes a sync cycle for the given event.
func (n *DefaultNode) Tick(ctx context.Context, event EventType) Action {
	switch event {
	case EvTimer:
		// BUGGIFY: skip a timer-triggered sync to test stale-state behavior.
		if n.buggify != nil && n.buggify.Check("node.tick.earlyReturn", 0.05) {
			return Action{Type: ActionNone}
		}
		result, err := n.runSync(ctx)
		return Action{Type: ActionSynced, Result: result, Err: err}
	case EvSyncNow:
		result, err := n.runSync(ctx)
		return Action{Type: ActionSynced, Result: result, Err: err}
	default:
		return Action{Type: ActionNone}
	}
}

// Repo returns the node's Fossil repository.
func (n *DefaultNode) Repo() *repo.Repo {
	return n.repo
}

func (n *DefaultNode) runSync(ctx context.Context) (*sync.SyncResult, error) {
	return sync.Sync(ctx, n.repo, n.transport, sync.SyncOpts{
		Push:        n.opts.Push,
		Pull:        n.opts.Pull,
		ProjectCode: n.projectCode,
		ServerCode:  n.serverCode,
		UV:          n.opts.UV,
		XTableSync:  n.opts.XTableSync,
		Private:     n.opts.Private,
		Buggify:     n.buggify,
	})
}
