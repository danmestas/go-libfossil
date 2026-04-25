package libfossil

import (
	"context"
	"fmt"
)

// PullOpts configures a pull-only sync. Fields are a subset of SyncOpts;
// Pull is hard-coded true and Push is hard-coded false to keep the API
// surface honest about what Pull does.
type PullOpts struct {
	// ProjectCode optionally pins the expected server project code. Empty
	// accepts whatever the peer advertises (matches existing Sync semantics).
	ProjectCode string

	// MaxSend caps the bytes the client will send per round (mostly clones
	// of clients with UV files). Zero leaves the existing default in place.
	MaxSend int

	// Observer receives sync-progress events. nil disables observation.
	Observer SyncObserver
}

// Pull fetches commits and ancillary objects from a Fossil HTTP peer and
// applies them to this repo. It is a strict pull — nothing is sent.
//
// Tiger Style: hostile inputs panic via assert at the boundary; transport
// failures return wrapped errors. Idempotent on a repo already at peer's
// tip (returns a SyncResult with Rounds=0–1 and FilesRecvd=0).
func (r *Repo) Pull(ctx context.Context, url string, opts PullOpts) (*SyncResult, error) {
	if ctx == nil {
		panic("libfossil: Pull: ctx is nil")
	}
	if url == "" {
		panic("libfossil: Pull: url is empty")
	}
	transport := NewHTTPTransport(url)
	res, err := r.Sync(ctx, transport, SyncOpts{
		Pull:        true,
		Push:        false,
		ProjectCode: opts.ProjectCode,
		MaxSend:     opts.MaxSend,
		Observer:    opts.Observer,
	})
	if err != nil {
		return res, fmt.Errorf("libfossil: pull %s: %w", url, err)
	}
	return res, nil
}
