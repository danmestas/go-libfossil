package checkout

import (
	"context"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ExtractStart describes the beginning of an extract or update operation.
type ExtractStart struct {
	Operation string          // "extract" or "update"
	TargetRID libfossil.FslID
}

// ExtractEnd describes the result of an extract or update operation.
type ExtractEnd struct {
	Operation    string
	TargetRID    libfossil.FslID
	FilesWritten int
	FilesRemoved int
	Conflicts    int
	Err          error
}

// ScanEnd describes the result of a ScanChanges operation.
type ScanEnd struct {
	FilesScanned int
	FilesChanged int
	FilesMissing int
	FilesExtra   int
}

// CommitStart describes the beginning of a commit operation.
type CommitStart struct {
	FilesEnqueued int
	Branch        string
	User          string
}

// CommitEnd describes the result of a commit operation.
type CommitEnd struct {
	RID         libfossil.FslID
	UUID        string
	FilesCommit int
	Err         error
}

// Observer receives lifecycle callbacks during checkout operations.
// A single Observer instance may be shared across multiple concurrent operations.
// Pass nil for no-op default.
//
// Performance: nopObserver implements all methods as empty functions.
// The only cost on the hot path is one indirect call per invocation (~2ns).
type Observer interface {
	// Extract/update lifecycle.
	ExtractStarted(ctx context.Context, e ExtractStart) context.Context
	ExtractFileCompleted(ctx context.Context, name string, change UpdateChange)
	ExtractCompleted(ctx context.Context, e ExtractEnd)

	// Scan lifecycle.
	ScanStarted(ctx context.Context) context.Context
	ScanCompleted(ctx context.Context, e ScanEnd)

	// Commit lifecycle.
	CommitStarted(ctx context.Context, e CommitStart) context.Context
	CommitCompleted(ctx context.Context, e CommitEnd)

	// Per-error recording — called on individual errors.
	Error(ctx context.Context, err error)
}

// nopObserver is the default observer that does nothing.
type nopObserver struct{}

func (nopObserver) ExtractStarted(ctx context.Context, _ ExtractStart) context.Context {
	return ctx
}
func (nopObserver) ExtractFileCompleted(_ context.Context, _ string, _ UpdateChange) {}
func (nopObserver) ExtractCompleted(_ context.Context, _ ExtractEnd)                 {}
func (nopObserver) ScanStarted(ctx context.Context) context.Context                  { return ctx }
func (nopObserver) ScanCompleted(_ context.Context, _ ScanEnd)                       {}
func (nopObserver) CommitStarted(ctx context.Context, _ CommitStart) context.Context {
	return ctx
}
func (nopObserver) CommitCompleted(_ context.Context, _ CommitEnd) {}
func (nopObserver) Error(_ context.Context, _ error)              {}

// resolveObserver returns obs if non-nil, otherwise nopObserver{}.
func resolveObserver(obs Observer) Observer {
	if obs == nil {
		return nopObserver{}
	}
	return obs
}
