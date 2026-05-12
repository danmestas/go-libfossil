package libfossil

import (
	"context"
	"fmt"
	"os"

	"github.com/danmestas/libfossil/internal/repo"
	internalsync "github.com/danmestas/libfossil/internal/sync"
	"github.com/danmestas/libfossil/simio"
)

// CreateOpts configures repository creation.
type CreateOpts struct {
	User string
	// ProjectCode optionally sets the repo's project-code. Empty
	// generates a fresh one (current behavior). When non-empty, must
	// be 40-char lowercase hex (^[0-9a-f]{40}$) — matching the format
	// of generated project-codes. Invalid values return an error
	// before any file is written.
	ProjectCode string
	// Rand provides random bytes for project-code and server-code generation.
	// Nil defaults to crypto/rand (production). Set to simio.NewSeededRand
	// for deterministic simulation testing.
	Rand simio.Rand
}

// Create creates a new Fossil repository at the given path.
func Create(path string, opts CreateOpts) (*Repo, error) {
	if opts.ProjectCode != "" && !isProjectCode40HexLower(opts.ProjectCode) {
		return nil, fmt.Errorf("libfossil: invalid CreateOpts.ProjectCode %q: must be 40-char lowercase hex", opts.ProjectCode)
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("libfossil: repository already exists: %s", path)
	}
	user := opts.User
	if user == "" {
		user = os.Getenv("USER")
	}
	rng := opts.Rand
	if rng == nil {
		rng = simio.CryptoRand{}
	}
	inner, err := repo.Create(path, user, rng, opts.ProjectCode)
	if err != nil {
		return nil, fmt.Errorf("libfossil: create: %w", err)
	}
	return &Repo{inner: inner, path: path}, nil
}

func isProjectCode40HexLower(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// Open opens an existing Fossil repository.
func Open(path string) (*Repo, error) {
	inner, err := repo.Open(path)
	if err != nil {
		return nil, fmt.Errorf("libfossil: open: %w", err)
	}
	return &Repo{inner: inner, path: path}, nil
}

// CloneResult reports what happened during a clone.
type CloneResult struct {
	Rounds          int
	BlobsRecvd      int
	ArtifactsLinked int
	ProjectCode     string
	ServerCode      string
	Messages        []string
}

// Clone performs a full repository clone from a remote Fossil server.
// It creates a new repository at the given path, runs the clone protocol
// until convergence, and returns the opened Repo handle and a result summary.
// On error, the partially-created repo file is removed.
func Clone(ctx context.Context, path string, t Transport, opts CloneOpts) (*Repo, *CloneResult, error) {
	adapter := &transportAdapter{pub: t}
	iOpts := internalsync.CloneOpts{
		User:     opts.User,
		Password: opts.Password,
		Observer: adaptObserver(opts.Observer),
		Buggify:  opts.Buggify,
	}
	inner, iResult, err := internalsync.Clone(ctx, path, adapter, iOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("libfossil: clone: %w", err)
	}
	result := &CloneResult{
		Rounds:          iResult.Rounds,
		BlobsRecvd:      iResult.BlobsRecvd,
		ArtifactsLinked: iResult.ArtifactsLinked,
		ProjectCode:     iResult.ProjectCode,
		ServerCode:      iResult.ServerCode,
		Messages:        iResult.Messages,
	}
	return &Repo{inner: inner, path: path}, result, nil
}
