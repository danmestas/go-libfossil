package cli

import (
	"errors"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil"
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
	return rid, nil
}
