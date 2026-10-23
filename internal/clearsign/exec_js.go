//go:build js

package clearsign

import (
	"context"
	"errors"
	"fmt"
)

// errNoProcesses: js/wasm runs no external signer or gpg. SSH signatures
// still verify, in process.
var errNoProcesses = errors.New("external signing commands are unavailable on js/wasm")

func runClearsign(context.Context, string, []byte) ([]byte, error) { return nil, errNoProcesses }

func runDetached(context.Context, string, []byte) ([]byte, error) { return nil, errNoProcesses }

func verifyPGP(context.Context, string, []byte) error {
	return fmt.Errorf("%w: %w", ErrCannotVerify, errNoProcesses)
}
