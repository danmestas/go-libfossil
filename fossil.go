package libfossil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/go-libfossil/internal/repo"
	internalsync "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/simio"
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
	if dir := filepath.Dir(path); !isDir(dir) {
		return nil, fmt.Errorf("libfossil: create %s: parent directory does not exist: %s", path, dir)
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

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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
	// Elided counts the artifacts left absent under the store cone, and
	// ConeServed the replies whose server acknowledged applying it (pragma
	// cone-served, draft-fossil-sparse-cone-00 §6.3). Both are zero for a
	// dense clone.
	Elided, ConeServed int
	// HorizonServed counts the replies whose server served the store's
	// horizon (pragma horizon-served, draft-fossil-horizon-00 §6.2.5); the
	// other rounds were classified locally. StructuralServed counts those
	// served at the structural level, and HorizonFallback reports a
	// structural clone that kept the content horizon instead (§6.3).
	HorizonServed    int
	StructuralServed int
	HorizonFallback  bool
	// HorizonHistoryURL is the interface outside the sync protocol at which
	// the origin says history below a horizon can be queried (pragma
	// horizon-served URL, draft-fossil-horizon-00 §6.2.5), the last a reply
	// named; "" when none did. An origin names it with
	// SettingHorizonHistoryURL.
	HorizonHistoryURL string
}

// Clone performs a full repository clone from a remote Fossil server.
// It creates a new repository at the given path, runs the clone protocol
// until convergence, and returns the opened Repo handle and a result summary.
// On error, the partially-created repo file is removed. Artifacts the clone
// rounds leave missing stay phantoms unless opts.PullPhantoms is set.
func Clone(ctx context.Context, path string, t Transport, opts CloneOpts) (*Repo, *CloneResult, error) {
	adapter := &transportAdapter{pub: t}
	iOpts := internalsync.CloneOpts{
		User:         opts.User,
		Password:     opts.Password,
		Observer:     adaptObserver(opts.Observer),
		Buggify:      opts.Buggify,
		PullPhantoms: opts.PullPhantoms,
		StoreCone:    unwrapCone(opts.StoreCone),
		SeedKey:      opts.SeedKey,
	}
	// A transport that names its peer keys the horizon cursor, as for Sync.
	if o, ok := t.(interface{ syncOrigin() string }); ok {
		iOpts.Origin = o.syncOrigin()
	}
	inner, iResult, err := internalsync.Clone(ctx, path, adapter, iOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("libfossil: clone: %w", err)
	}
	result := &CloneResult{
		Rounds:            iResult.Rounds,
		BlobsRecvd:        iResult.BlobsRecvd,
		ArtifactsLinked:   iResult.ArtifactsLinked,
		ProjectCode:       iResult.ProjectCode,
		ServerCode:        iResult.ServerCode,
		Messages:          iResult.Messages,
		Elided:            iResult.Elided,
		ConeServed:        iResult.ConeServed,
		HorizonServed:     iResult.HorizonServed,
		StructuralServed:  iResult.StructuralServed,
		HorizonFallback:   iResult.HorizonFallback,
		HorizonHistoryURL: iResult.HorizonHistoryURL,
	}
	return &Repo{inner: inner, path: path}, result, nil
}
