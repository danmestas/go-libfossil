package libfossil

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/simio"
)

// Checkout represents a working directory linked to a Fossil repository.
// A Checkout is not safe for concurrent use. Callers must serialize
// access to a single Checkout instance.
type Checkout struct {
	inner *checkout.Checkout
}

// CheckoutCreateOpts configures creating a new checkout.
type CheckoutCreateOpts struct {
	Env      *simio.Env       // nil = real env
	Observer CheckoutObserver // nil = nop
}

// CheckoutOpenOpts configures opening an existing checkout.
type CheckoutOpenOpts struct {
	SearchParents bool       // search parent dirs for .fslckout
	Env           *simio.Env // nil = real env
	Observer      CheckoutObserver
}

// ExtractOpts configures file extraction from a checkin.
type ExtractOpts struct {
	Force bool // overwrite uncommitted changes
}

// UpdateOpts configures updating to a new version with merge.
type UpdateOpts struct {
	TargetRID int64 // 0 = tip
}

// UpdateResult reports what Update actually did. Paths, not counts: a
// caller needs to know which files to show a user, not merely how many
// changed. All three slices are sorted (sort.Strings) before Update
// returns, so the order is deterministic — safe for golden-file tests or
// reflect.DeepEqual — rather than depending on Go's randomized map
// iteration order.
//
// Outcome mapping (Update's error return is the other half of this):
//
//	clean       - err == nil, Conflicted empty
//	conflicted  - err == nil, Conflicted non-empty
//	failed      - err != nil, UpdateResult is the zero value
//
// A non-empty Conflicted means three-way merge could not resolve those
// files cleanly and wrote "<<<<<<<"-style conflict markers directly into
// them on disk. That is NOT an error — Update still returns a nil error,
// because the update itself succeeded and the working tree is usable; it
// just isn't clean. A caller that only checks the returned error gets
// today's pre-conflict-detection behavior. A caller that needs to warn a
// user about marker text in their files must check Conflicted. Every path
// in Conflicted also appears in FilesWritten: the marker text is what was
// written there.
type UpdateResult struct {
	FilesWritten []string // paths written (added, updated, or merged, clean or not); includes every Conflicted path
	FilesRemoved []string // paths deleted from the working tree
	Conflicted   []string // paths that now contain conflict markers — not an error; also present in FilesWritten
}

// RevertOpts configures reverting file changes.
type RevertOpts struct {
	Files []string // empty = revert all
}

// CommitOpts configures creating a checkin from the checkout.
// (This type is distinct from the existing libfossil.CommitOpts which takes
// explicit file content for direct repo commits without a checkout.)
type CheckoutCommitOpts struct {
	Message string
	User    string
	Branch  string   // empty = current branch
	Tags    []string // additional tag names
	Delta   bool
}

// CheckoutChange describes a single file change in the checkout.
type CheckoutChange struct {
	Name   string
	Change string // "added", "modified", "deleted", "renamed", "missing"
}

// CreateCheckout creates a new checkout directory linked to this repository.
// The directory is created if it does not exist. The checkout is initialized
// to the tip checkin.
func (r *Repo) CreateCheckout(dir string, opts CheckoutCreateOpts) (*Checkout, error) {
	inner, err := checkout.Create(r.inner, dir, checkout.CreateOpts{
		Env:      opts.Env,
		Observer: adaptCheckoutObserver(opts.Observer),
	})
	if err != nil {
		return nil, fmt.Errorf("libfossil: create checkout: %w", err)
	}
	return &Checkout{inner: inner}, nil
}

// OpenCheckout opens an existing checkout directory linked to this repository.
func (r *Repo) OpenCheckout(dir string, opts CheckoutOpenOpts) (*Checkout, error) {
	inner, err := checkout.Open(r.inner, dir, checkout.OpenOpts{
		SearchParents: opts.SearchParents,
		Env:           opts.Env,
		Observer:      adaptCheckoutObserver(opts.Observer),
	})
	if err != nil {
		return nil, fmt.Errorf("libfossil: open checkout: %w", err)
	}
	return &Checkout{inner: inner}, nil
}

// Close closes the checkout database. Does NOT close the repo.
func (c *Checkout) Close() error {
	if c == nil || c.inner == nil {
		return nil
	}
	return c.inner.Close()
}

// Dir returns the checkout directory path.
func (c *Checkout) Dir() string {
	return c.inner.Dir()
}

// Version returns the current checkout version (RID and UUID).
func (c *Checkout) Version() (int64, string, error) {
	rid, uuid, err := c.inner.Version()
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: checkout version: %w", err)
	}
	return int64(rid), uuid, nil
}

// Extract writes files from the specified checkin to the working directory.
func (c *Checkout) Extract(rid int64, opts ExtractOpts) error {
	err := c.inner.Extract(fsltype.FslID(rid), checkout.ExtractOpts{
		Force: opts.Force,
	})
	if err != nil {
		return fmt.Errorf("libfossil: extract: %w", err)
	}
	return nil
}

// Update updates the checkout to a new version, performing 3-way merge
// where needed to preserve local modifications. See UpdateResult for how to
// distinguish a clean update from one that wrote conflict markers into
// working-tree files.
func (c *Checkout) Update(opts UpdateOpts) (UpdateResult, error) {
	res, err := c.inner.Update(checkout.UpdateOpts{
		TargetRID: fsltype.FslID(opts.TargetRID),
	})
	if err != nil {
		return UpdateResult{}, fmt.Errorf("libfossil: update: %w", err)
	}
	return UpdateResult{
		FilesWritten: res.FilesWritten,
		FilesRemoved: res.FilesRemoved,
		Conflicted:   res.Conflicted,
	}, nil
}

// HasChanges returns true if the checkout has any modified, deleted, or
// renamed files. This is a DB-only check; call Extract or scan first to
// detect on-disk modifications.
func (c *Checkout) HasChanges() (bool, error) {
	has, err := c.inner.HasChanges()
	if err != nil {
		return false, fmt.Errorf("libfossil: has changes: %w", err)
	}
	return has, nil
}

// Status scans the working directory for changes and returns a list of
// changed files. Wraps ScanChanges + VisitChanges.
func (c *Checkout) Status() ([]CheckoutChange, error) {
	rid, _, err := c.inner.Version()
	if err != nil {
		return nil, fmt.Errorf("libfossil: status: %w", err)
	}

	var changes []CheckoutChange
	err = c.inner.VisitChanges(rid, true, func(entry checkout.ChangeEntry) error {
		changes = append(changes, CheckoutChange{
			Name:   entry.Name,
			Change: fileChangeString(entry.Change),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("libfossil: status: %w", err)
	}
	return changes, nil
}

// Add adds files to version tracking. Returns the number of files added.
// Files already tracked are silently skipped.
func (c *Checkout) Add(patterns []string) (int, error) {
	counts, err := c.inner.Manage(checkout.ManageOpts{
		Paths: patterns,
	})
	if err != nil {
		return 0, fmt.Errorf("libfossil: add: %w", err)
	}
	return counts.Added, nil
}

// Remove removes files from version tracking. Newly added files are
// deleted from vfile; committed files are marked as deleted.
func (c *Checkout) Remove(patterns []string) error {
	err := c.inner.Unmanage(checkout.UnmanageOpts{
		Paths: patterns,
	})
	if err != nil {
		return fmt.Errorf("libfossil: remove: %w", err)
	}
	return nil
}

// Rename marks a tracked file as renamed and moves it on disk.
func (c *Checkout) Rename(oldName, newName string) error {
	err := c.inner.Rename(checkout.RenameOpts{
		From:     oldName,
		To:       newName,
		DoFsMove: true,
	})
	if err != nil {
		return fmt.Errorf("libfossil: rename: %w", err)
	}
	return nil
}

// Revert restores files to their checkout version state.
// If opts.Files is empty, reverts all changed files.
func (c *Checkout) Revert(opts RevertOpts) error {
	err := c.inner.Revert(checkout.RevertOpts{
		Paths: opts.Files,
	})
	if err != nil {
		return fmt.Errorf("libfossil: revert: %w", err)
	}
	return nil
}

// Checkin creates a new checkin from the checkout working directory.
// Returns the RID and UUID of the new checkin manifest.
func (c *Checkout) Checkin(opts CheckoutCommitOpts) (int64, string, error) {
	rid, uuid, err := c.inner.Commit(checkout.CommitOpts{
		Message: opts.Message,
		User:    opts.User,
		Branch:  opts.Branch,
		Tags:    opts.Tags,
		Delta:   opts.Delta,
	})
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: checkin: %w", err)
	}
	return int64(rid), uuid, nil
}

// WouldFork reports whether committing on the current branch would create
// a fork. Returns true when another leaf exists on the same branch.
func (c *Checkout) WouldFork() (bool, error) {
	fork, err := c.inner.WouldFork()
	if err != nil {
		return false, fmt.Errorf("libfossil: would fork: %w", err)
	}
	return fork, nil
}

// fileChangeString converts an internal FileChange enum to a human-readable string.
func fileChangeString(fc checkout.FileChange) string {
	switch fc {
	case checkout.ChangeAdded:
		return "added"
	case checkout.ChangeRemoved:
		return "deleted"
	case checkout.ChangeMissing:
		return "missing"
	case checkout.ChangeRenamed:
		return "renamed"
	case checkout.ChangeModified:
		return "modified"
	default:
		return "unknown"
	}
}

// checkoutObserverAdapter bridges the public CheckoutObserver to the internal
// checkout.Observer (which carries context.Context).
type checkoutObserverAdapter struct {
	pub CheckoutObserver
}

func (a *checkoutObserverAdapter) ExtractStarted(ctx context.Context, e checkout.ExtractStart) context.Context {
	a.pub.ExtractStarted(ExtractStart{
		RID: int64(e.TargetRID),
	})
	return ctx
}

func (a *checkoutObserverAdapter) ExtractFileCompleted(_ context.Context, name string, change checkout.UpdateChange) {
	a.pub.ExtractFileCompleted(name, convertUpdateChange(change))
}

func (a *checkoutObserverAdapter) ExtractCompleted(_ context.Context, e checkout.ExtractEnd) {
	a.pub.ExtractCompleted(ExtractEnd{
		FilesWritten: e.FilesWritten,
	})
}

func (a *checkoutObserverAdapter) ScanStarted(ctx context.Context) context.Context {
	a.pub.ScanStarted("")
	return ctx
}

func (a *checkoutObserverAdapter) ScanCompleted(_ context.Context, e checkout.ScanEnd) {
	a.pub.ScanCompleted(ScanEnd{
		FilesScanned: e.FilesScanned,
	})
}

func (a *checkoutObserverAdapter) CommitStarted(ctx context.Context, e checkout.CommitStart) context.Context {
	a.pub.CommitStarted(CommitStart{
		User:  e.User,
		Files: e.FilesEnqueued,
	})
	return ctx
}

func (a *checkoutObserverAdapter) CommitCompleted(_ context.Context, e checkout.CommitEnd) {
	a.pub.CommitCompleted(CommitEnd{
		UUID: e.UUID,
		RID:  int64(e.RID),
	})
}

func (a *checkoutObserverAdapter) Error(_ context.Context, err error) {
	a.pub.Error(err)
}

// adaptCheckoutObserver returns an internal checkout.Observer wrapping the
// public CheckoutObserver. Returns nil if pub is nil.
func adaptCheckoutObserver(pub CheckoutObserver) checkout.Observer {
	if pub == nil {
		return nil
	}
	return &checkoutObserverAdapter{pub: pub}
}

// convertUpdateChange maps internal checkout.UpdateChange to public UpdateChange.
func convertUpdateChange(c checkout.UpdateChange) UpdateChange {
	switch c {
	case checkout.UpdateAdded, checkout.UpdateAddPropagated:
		return ChangeAdded
	case checkout.UpdateRemoved, checkout.UpdateRmPropagated:
		return ChangeDeleted
	case checkout.UpdateUpdated, checkout.UpdateUpdatedBinary,
		checkout.UpdateMerged, checkout.UpdateEdited:
		return ChangeModified
	default:
		return ChangeModified
	}
}
