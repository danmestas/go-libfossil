package libfossil

import (
	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// Repo is an opaque handle to a Fossil repository.
type Repo struct {
	inner *repo.Repo
	path  string
}

// CheckpointMode mirrors SQLite's PRAGMA wal_checkpoint(<mode>) argument.
type CheckpointMode = db.CheckpointMode

const (
	CheckpointPassive  = db.CheckpointPassive
	CheckpointFull     = db.CheckpointFull
	CheckpointRestart  = db.CheckpointRestart
	CheckpointTruncate = db.CheckpointTruncate
)

// Path returns the filesystem path to the repository file.
func (r *Repo) Path() string { return r.path }

// Inner returns the underlying internal repo handle.
// This is exported for use by in-module packages (e.g., cli/) that need
// direct access to the repo DB for raw SQL or internal package calls.
func (r *Repo) Inner() *repo.Repo { return r.inner }

// Close closes the repository and releases resources. As part of close,
// a WAL TRUNCATE checkpoint is run so the on-disk repo file is readable
// by external fossil/SQLite tooling.
func (r *Repo) Close() error {
	if r.inner == nil {
		return nil
	}
	return r.inner.Close()
}

// Checkpoint runs PRAGMA wal_checkpoint(<mode>) against the repository.
// Safe to call on a live repo. CheckpointPassive is non-blocking and
// appropriate for periodic background checkpoints. CheckpointTruncate
// produces a maximally compact on-disk file readable by external fossil
// tooling without a subsequent Close.
func (r *Repo) Checkpoint(mode CheckpointMode) error {
	if r.inner == nil {
		return nil
	}
	return r.inner.Checkpoint(mode)
}

// DB returns the underlying database handle for raw SQL queries.
// Use this when the high-level Repo methods don't cover your use case.
func (r *Repo) DB() *db.DB { return r.inner.DB() }

// WithTx executes fn within a database transaction.
func (r *Repo) WithTx(fn func(tx *db.Tx) error) error { return r.inner.WithTx(fn) }

// Verify checks repository integrity (blob checksums, delta chains).
func (r *Repo) Verify() error {
	return r.inner.Verify()
}
