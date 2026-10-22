package cli

import (
	"context"
	"errors"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// Exit statuses the machine-facing commands (snapshot, log, cat --at, diff)
// document and keep stable. Every other failure, including a usage error, is
// ExitFailure.
const (
	ExitOK       = 0
	ExitFailure  = 1
	ExitNotFound = 2 // the version, path or history asked for does not exist
)

// ExitError is a command failure that carries the process exit status it
// maps to. The binary's main exits with Code after printing Err.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// notFound marks err as an ExitNotFound failure.
func notFound(err error) error {
	return &ExitError{Code: ExitNotFound, Err: err}
}

// resolveVersionExit is resolveRID with a version that does not exist
// reported as ExitNotFound.
func resolveVersionExit(r *libfossil.Repo, version string) (int64, error) {
	rid, err := resolveRID(r, version)
	if errors.Is(err, libfossil.ErrVersionNotFound) {
		return 0, notFound(err)
	}
	if err != nil {
		return 0, fmt.Errorf("resolving %q: %w", version, err)
	}
	return rid, crossesHorizon(r, rid)
}

// resolveCheckin is resolveRID for an operation that reads the check-in's
// tree, which it cannot do across a structural horizon.
func resolveCheckin(r *libfossil.Repo, version string) (int64, error) {
	rid, err := resolveRID(r, version)
	if err != nil {
		return 0, err
	}
	return rid, crossesHorizon(r, rid)
}

// crossesHorizon refuses a check-in below the store's structural horizon,
// naming the horizon and the remedy (draft-fossil-horizon-00 §5.5): its
// manifest is not held, and a deepen to it is what brings it.
func crossesHorizon(r *libfossil.Repo, rid int64) error {
	h, below, err := content.BelowHorizon(context.Background(), r.Inner().DB(), fsltype.FslID(rid))
	if err != nil || !below {
		return err
	}
	uuid, err := r.UUIDFromRID(rid)
	if err != nil {
		return err
	}
	return fmt.Errorf("%s is below the structural horizon at %s: its history is not held here; "+
		"deepen the horizon to it (repo cone store --horizon %s --structural) to reach it", uuid, h.Hash(), uuid)
}
