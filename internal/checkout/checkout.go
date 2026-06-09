// Package checkout provides working directory management for Fossil repositories.
// It implements file extraction, change tracking, staging, and commit operations,
// ported from libfossil's checkout API. All filesystem operations go through
// simio.Env for platform-agnostic operation (native, WASI, browser WASM).
//
// A Checkout is not safe for concurrent use. Callers must serialize
// access to a single Checkout instance.
package checkout

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/danmestas/libfossil/db"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
)

// Checkout represents an opened or created checkout database.
// A Checkout owns the checkout DB (*sql.DB) but NOT the repo — the caller
// owns the repo lifecycle.
type Checkout struct {
	db           *sql.DB
	repo         *repo.Repo
	env          *simio.Env
	obs          Observer
	dir          string
	checkinQueue map[string]bool // in-memory staging queue (session-scoped)
}

// initCheckoutVersion finds the tip checkin from the repo and sets
// vvar checkout/checkout-hash in the checkout DB. Empty repos use the
// unborn checkout state: checkout=0 with empty checkout-hash.
func initCheckoutVersion(ckdb *sql.DB, r *repo.Repo) error {
	var tipRID int64
	var tipUUID string
	err := r.DB().QueryRow(`
		SELECT l.rid, b.uuid FROM leaf l
		JOIN event e ON e.objid=l.rid
		JOIN blob b ON b.rid=l.rid
		WHERE e.type='ci'
		ORDER BY e.mtime DESC LIMIT 1
	`).Scan(&tipRID, &tipUUID)
	if err != nil {
		if err == sql.ErrNoRows {
			return initUnbornCheckoutVersion(ckdb)
		}
		return fmt.Errorf("checkout.Create: query tip: %w", err)
	}

	if err := setVVar(ckdb, "checkout", strconv.FormatInt(tipRID, 10)); err != nil {
		return fmt.Errorf("checkout.Create: %w", err)
	}
	if err := setVVar(ckdb, "checkout-hash", tipUUID); err != nil {
		return fmt.Errorf("checkout.Create: %w", err)
	}
	return nil
}

func initUnbornCheckoutVersion(ckdb *sql.DB) error {
	if err := setVVar(ckdb, "checkout", "0"); err != nil {
		return fmt.Errorf("checkout.Create: %w", err)
	}
	if err := setVVar(ckdb, "checkout-hash", ""); err != nil {
		return fmt.Errorf("checkout.Create: %w", err)
	}
	return nil
}
func initUndoState(ckdb *sql.DB) error {
	if err := setVVar(ckdb, "undo_available", "0"); err != nil {
		return fmt.Errorf("checkout.Create: %w", err)
	}
	if err := setVVar(ckdb, "undo_checkout", "0"); err != nil {
		return fmt.Errorf("checkout.Create: %w", err)
	}
	return nil
}

// Create creates a new checkout database at dir/.fslckout (or dir/_FOSSIL_ on Windows),
// finds the tip checkin from the repo, and sets vvar checkout/checkout-hash.
//
// Panics if r or dir is nil/empty (TigerStyle precondition).
func Create(r *repo.Repo, dir string, opts CreateOpts) (*Checkout, error) {
	if r == nil {
		panic("checkout.Create: nil *repo.Repo")
	}
	if dir == "" {
		panic("checkout.Create: empty dir")
	}

	env := opts.Env
	if env == nil {
		env = simio.RealEnv()
	}
	if env.Storage == nil {
		env.Storage = simio.OSStorage{}
	}

	obs := resolveObserver(opts.Observer)

	// Verify a SQLite driver is registered before creating the checkout DB.
	if db.RegisteredDriver() == nil {
		return nil, fmt.Errorf("checkout.Create: no SQLite driver registered")
	}

	// Determine DB path
	dbPath := filepath.Join(dir, PreferredDBName())

	// Create parent directory if needed
	if err := env.Storage.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("checkout.Create: mkdir: %w", err)
	}

	// Open the checkout database
	ckdb, err := db.OpenSQL(dbPath, db.OpenConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("checkout.Create: sql.Open: %w", err)
	}

	// Ensure tables exist
	if err := EnsureTables(ckdb); err != nil {
		ckdb.Close()
		return nil, fmt.Errorf("checkout.Create: %w", err)
	}

	// Find the tip checkin and set vvar checkout/checkout-hash
	if err := initCheckoutVersion(ckdb, r); err != nil {
		ckdb.Close()
		return nil, err
	}
	if err := initUndoState(ckdb); err != nil {
		ckdb.Close()
		return nil, err
	}

	// Set the repository vvar so tools can find the repo.
	if err := setVVar(ckdb, "repository", r.Path()); err != nil {
		ckdb.Close()
		return nil, fmt.Errorf("checkout.Create: %w", err)
	}

	c := &Checkout{
		db:   ckdb,
		repo: r,
		env:  env,
		obs:  obs,
		dir:  dir,
	}
	rid, _, err := c.Version()
	if err != nil {
		ckdb.Close()
		return nil, err
	}
	if rid != 0 {
		if _, err := c.LoadVFile(rid, true); err != nil {
			ckdb.Close()
			return nil, err
		}
	}
	return c, nil
}

// Open opens an existing checkout database. If opts.SearchParents is true,
// it searches parent directories for the checkout DB using FindCheckoutDB.
//
// Panics if r or dir is nil/empty (TigerStyle precondition).
func Open(r *repo.Repo, dir string, opts OpenOpts) (*Checkout, error) {
	if r == nil {
		panic("checkout.Open: nil *repo.Repo")
	}
	if dir == "" {
		panic("checkout.Open: empty dir")
	}

	env := opts.Env
	if env == nil {
		env = simio.RealEnv()
	}
	if env.Storage == nil {
		env.Storage = simio.OSStorage{}
	}

	obs := resolveObserver(opts.Observer)

	// Verify a SQLite driver is registered before opening the checkout DB.
	if db.RegisteredDriver() == nil {
		return nil, fmt.Errorf("checkout.Open: no SQLite driver registered")
	}

	// Find the checkout DB
	dbPath, err := FindCheckoutDB(env.Storage, dir, opts.SearchParents)
	if err != nil {
		return nil, fmt.Errorf("checkout.Open: %w", err)
	}

	// Open the checkout database
	ckdb, err := db.OpenSQL(dbPath, db.OpenConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("checkout.Open: sql.Open: %w", err)
	}

	// Ping to validate the connection
	if err := ckdb.Ping(); err != nil {
		ckdb.Close()
		return nil, fmt.Errorf("checkout.Open: ping: %w", err)
	}

	// Verify vvar checkout exists
	checkoutVal, err := getVVar(ckdb, "checkout")
	if err != nil {
		ckdb.Close()
		return nil, fmt.Errorf("checkout.Open: %w", err)
	}
	if checkoutVal == "" {
		ckdb.Close()
		return nil, fmt.Errorf("checkout.Open: vvar checkout not found")
	}

	c := &Checkout{
		db:   ckdb,
		repo: r,
		env:  env,
		obs:  obs,
		dir:  filepath.Dir(dbPath),
	}
	if err := c.ValidateFingerprint(); err != nil {
		ckdb.Close()
		return nil, fmt.Errorf("checkout.Open: %w", err)
	}
	return c, nil
}

// Close closes the checkout database. Does NOT close the repo.
func (c *Checkout) Close() error {
	if c == nil {
		panic("checkout.Close: nil *Checkout")
	}
	if c.db == nil {
		panic("checkout.Close: nil *sql.DB (already closed?)")
	}
	return c.db.Close()
}

// Dir returns the checkout directory.
func (c *Checkout) Dir() string {
	if c == nil {
		panic("checkout.Dir: nil *Checkout")
	}
	return c.dir
}

// Repo returns the associated repo.
func (c *Checkout) Repo() *repo.Repo {
	if c == nil {
		panic("checkout.Repo: nil *Checkout")
	}
	return c.repo
}

// Version returns the current checkout version (RID and UUID).
func (c *Checkout) Version() (libfossil.FslID, string, error) {
	if c == nil {
		panic("checkout.Version: nil *Checkout")
	}

	ridStr, err := getVVar(c.db, "checkout")
	if err != nil {
		return 0, "", fmt.Errorf("checkout.Version: %w", err)
	}
	if ridStr == "" {
		return 0, "", fmt.Errorf("checkout.Version: vvar checkout not set")
	}

	rid64, err := strconv.ParseInt(ridStr, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("checkout.Version: parse RID: %w", err)
	}
	if rid64 < 0 {
		return 0, "", fmt.Errorf("checkout.Version: negative checkout RID %d", rid64)
	}

	uuid, err := getVVar(c.db, "checkout-hash")
	if err != nil {
		return 0, "", fmt.Errorf("checkout.Version: %w", err)
	}
	if rid64 != 0 && uuid == "" {
		return 0, "", fmt.Errorf("checkout.Version: vvar checkout-hash not set")
	}
	if rid64 == 0 && uuid != "" {
		return 0, "", fmt.Errorf("checkout.Version: unborn checkout has non-empty checkout-hash")
	}

	return libfossil.FslID(rid64), uuid, nil
}

// ValidateFingerprint verifies that the checkout-hash vvar matches the blob uuid in the repo.
func (c *Checkout) ValidateFingerprint() error {
	if c == nil {
		panic("checkout.ValidateFingerprint: nil *Checkout")
	}

	rid, uuid, err := c.Version()
	if err != nil {
		return fmt.Errorf("checkout.ValidateFingerprint: %w", err)
	}

	if rid == 0 {
		if uuid != "" {
			return fmt.Errorf("checkout.ValidateFingerprint: unborn checkout has non-empty checkout-hash")
		}
		return nil
	}

	// Query the repo for the blob uuid
	var repoUUID string
	err = c.repo.DB().QueryRow("SELECT uuid FROM blob WHERE rid = ?", rid).Scan(&repoUUID)
	if err != nil {
		return fmt.Errorf("checkout.ValidateFingerprint: query blob: %w", err)
	}

	if repoUUID != uuid {
		return fmt.Errorf(
			"checkout.ValidateFingerprint: mismatch (checkout: %s, repo: %s)",
			uuid, repoUUID,
		)
	}

	return nil
}

// maxParentSearchDepth is the maximum number of parent directories to search.
const maxParentSearchDepth = 256

// FindCheckoutDB searches for a checkout database (.fslckout or _FOSSIL_) in dir,
// and optionally in parent directories if searchParents is true.
// Returns the full path to the DB file.
func FindCheckoutDB(storage simio.Storage, dir string, searchParents bool) (string, error) {
	if storage == nil {
		panic("checkout.FindCheckoutDB: nil Storage")
	}
	if dir == "" {
		panic("checkout.FindCheckoutDB: empty dir")
	}

	currentDir := dir
	for depth := 0; depth <= maxParentSearchDepth; depth++ {
		// Try each DB name
		for _, name := range DBNames() {
			dbPath := filepath.Join(currentDir, name)
			if _, err := storage.Stat(dbPath); err == nil {
				return dbPath, nil
			}
		}

		if !searchParents {
			break
		}

		// Move to parent directory
		parentDir := filepath.Dir(currentDir)
		if parentDir == currentDir {
			// Reached the root
			break
		}
		currentDir = parentDir
	}

	return "", fmt.Errorf("checkout.FindCheckoutDB: no checkout DB found in %s", dir)
}

// PreferredDBName returns the preferred checkout DB name based on the OS.
// Returns "_FOSSIL_" on Windows, ".fslckout" elsewhere.
func PreferredDBName() string {
	if runtime.GOOS == "windows" {
		return "_FOSSIL_"
	}
	return ".fslckout"
}

// DBNames returns all possible checkout DB names, with the preferred name first.
func DBNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"_FOSSIL_", ".fslckout"}
	}
	return []string{".fslckout", "_FOSSIL_"}
}
