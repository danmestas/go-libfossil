package libfossil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"github.com/danmestas/go-libfossil/internal/cone"

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
	if err := checkParentDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("libfossil: create %s: %w", path, err)
	}
	user := opts.User
	if user == "" {
		user = defaultLogin()
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

// defaultLogin is the admin login a new repository seeds when CreateOpts.User
// is empty, chosen as fossil(1)'s db_create_default_users chooses it: the
// first of FOSSIL_USER, USER, LOGNAME and USERNAME that is set, else "root".
// Fossil consults its global default-user setting first; a library has no
// global config, so that step is skipped. A variable set to the empty string
// counts as unset. The result is never empty, so a process with no user in
// its environment, as a container or a CI runner often is, still creates a
// repository instead of failing on an empty login.
func defaultLogin() string {
	for _, name := range []string{"FOSSIL_USER", "USER", "LOGNAME", "USERNAME"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return "root"
}

// statParent is os.Stat; a test replaces it to stand in for a filesystem
// that cannot answer.
var statParent = os.Stat

// checkParentDir refuses dir only when the filesystem confirms it cannot
// hold a new repository: it does not exist, or it is not a directory. Any
// other stat error means the filesystem cannot say, as under TinyGo wasm,
// where the repository lives in a SQLite VFS and os.Stat is unsupported;
// then SQLite's own open reports a real failure.
func checkParentDir(dir string) error {
	info, err := statParent(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("parent directory does not exist: %s", dir)
	}
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("parent is not a directory: %s", dir)
	}
	return nil
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
	// HorizonRemedy is set when HorizonFallback is: it names the horizon,
	// says the manifests below it are now wanted, and names where they are
	// held, the dense origin at HorizonHistoryURL when a reply named one. A
	// peer that is itself at a horizon does not hold them
	// (draft-fossil-horizon-00 §6.2.3, §6.3, §6.4).
	HorizonRemedy string
	// HorizonResolved is the check-in CloneOpts.Horizon named resolved to and
	// HorizonSteps the steps back it took, fewer than HorizonDepth when
	// history ran out; HorizonResolvedLocally reports that the origin did
	// not resolve the name and the clone did, at the content horizon.
	HorizonResolved        string
	HorizonSteps           int
	HorizonResolvedLocally bool
}

// Clone performs a full repository clone from a remote Fossil server.
// It creates a new repository at the given path, runs the clone protocol
// until convergence, and returns the opened Repo handle and a result summary.
// On error, the partially-created repo file is removed. Artifacts the clone
// rounds leave missing stay phantoms unless opts.PullPhantoms is set.
func Clone(ctx context.Context, path string, t Transport, opts CloneOpts) (*Repo, *CloneResult, error) {
	adapter := &transportAdapter{pub: t}
	storeCone, named, err := cloneHorizon(opts)
	if err != nil {
		return nil, nil, fmt.Errorf("libfossil: clone: %w", err)
	}
	iOpts := internalsync.CloneOpts{
		User:         opts.User,
		Password:     opts.Password,
		Observer:     adaptObserver(opts.Observer),
		Buggify:      opts.Buggify,
		PullPhantoms: opts.PullPhantoms,
		StoreCone:    storeCone,
		SeedKey:      opts.SeedKey,
		Named:        named,
	}
	// A transport that names its peer keys the horizon cursor, as for Sync.
	iOpts.Origin = syncOrigin(t)
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
		HorizonRemedy:     iResult.HorizonRemedy,

		HorizonResolved:        iResult.HorizonResolved,
		HorizonSteps:           int(iResult.HorizonSteps),
		HorizonResolvedLocally: iResult.HorizonResolvedLocally,
	}
	return &Repo{inner: inner, path: path}, result, nil
}

// cloneHorizon splits a clone's horizon into the store cone it starts with
// and the horizon the origin is asked to resolve. A Horizon is always sent
// by name, a full hash included: the origin tries a full hash as a
// check-in and then as a symbolic name (§6.2.7), so a branch spelled as 40
// hex digits is found, and a peer that resolves no name is covered by the
// local fallback. Only a horizon given on StoreCone goes out as the hash.
func cloneHorizon(opts CloneOpts) (*cone.Cone, *internalsync.NamedHorizon, error) {
	sc := unwrapCone(opts.StoreCone)
	if opts.HorizonDepth < 0 || int64(opts.HorizonDepth) > math.MaxUint32 {
		return nil, nil, fmt.Errorf("horizon depth %d out of range", opts.HorizonDepth)
	}
	h := opts.Horizon
	if h == nil {
		if opts.HorizonDepth != 0 {
			return nil, nil, fmt.Errorf("a horizon depth needs a Horizon")
		}
		return sc, nil, nil
	}
	if _, ok := horizonOf(sc); ok {
		return nil, nil, fmt.Errorf("a horizon on both StoreCone and Horizon")
	}
	if h.Checkin == "" {
		return nil, nil, fmt.Errorf("a Horizon with no check-in")
	}
	level := cone.DepthContent
	if h.Structural {
		level = cone.DepthStructural
	}
	return sc, &internalsync.NamedHorizon{Name: h.Checkin, Depth: uint32(opts.HorizonDepth),
		Rule: cone.HorizonRule(h.Rule), Level: level}, nil
}

// horizonOf is c's horizon, ok false for nil or a cone without one.
func horizonOf(c *cone.Cone) (cone.Horizon, bool) {
	if c == nil {
		return cone.Horizon{}, false
	}
	return c.Horizon()
}
