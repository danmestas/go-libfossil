package repo

import (
	"fmt"

	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/content"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/simio"
)

type Repo struct {
	db   *db.DB
	path string
}

// Create creates a new repo at path, seeded with the given user.
// If projectCode is empty, a fresh project-code is generated; otherwise
// projectCode must already be valid (40-char lowercase hex) — validation
// is enforced at the libfossil.Create boundary.
func Create(path string, user string, rng simio.Rand, projectCode string) (*Repo, error) {
	env := simio.RealEnv()
	env.Rand = rng
	return CreateWithEnv(path, user, env, projectCode)
}

func CreateWithEnv(path string, user string, env *simio.Env, projectCode string) (*Repo, error) {
	if path == "" {
		panic("repo.CreateWithEnv: path must not be empty")
	}
	if env == nil {
		env = simio.RealEnv()
	}
	if env.Rand == nil {
		panic("repo.CreateWithEnv: env.Rand must not be nil")
	}
	if env.Storage == nil {
		env.Storage = simio.OSStorage{}
	}

	if _, err := env.Storage.Stat(path); err == nil {
		return nil, fmt.Errorf("repo.CreateWithEnv: file already exists: %s", path)
	}

	d, err := db.Open(path)
	if err != nil {
		return nil, fmt.Errorf("repo.CreateWithEnv open: %w", err)
	}

	if err := db.CreateRepoSchema(d); err != nil {
		d.Close()
		if rmErr := env.Storage.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("repo.CreateWithEnv: %w (cleanup failed: %v)", err, rmErr)
		}
		return nil, fmt.Errorf("repo.CreateWithEnv schema: %w", err)
	}

	if err := db.SeedUser(d, user); err != nil {
		d.Close()
		if rmErr := env.Storage.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("repo.CreateWithEnv: %w (cleanup failed: %v)", err, rmErr)
		}
		return nil, fmt.Errorf("repo.CreateWithEnv seed user: %w", err)
	}

	if err := db.SeedNobody(d, "cghijknorswy"); err != nil {
		d.Close()
		if rmErr := env.Storage.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("repo.CreateWithEnv: %w (cleanup failed: %v)", err, rmErr)
		}
		return nil, fmt.Errorf("repo.CreateWithEnv seed nobody: %w", err)
	}

	if err := db.SeedConfig(d, env.Rand, projectCode); err != nil {
		d.Close()
		if rmErr := env.Storage.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("repo.CreateWithEnv: %w (cleanup failed: %v)", err, rmErr)
		}
		return nil, fmt.Errorf("repo.CreateWithEnv seed config: %w", err)
	}

	return &Repo{db: d, path: path}, nil
}

func Open(path string) (*Repo, error) {
	return OpenWithEnv(path, nil)
}

func OpenWithEnv(path string, env *simio.Env) (*Repo, error) {
	if path == "" {
		panic("repo.OpenWithEnv: path must not be empty")
	}
	if env == nil {
		env = simio.RealEnv()
	}
	if env.Storage == nil {
		env.Storage = simio.OSStorage{}
	}

	if err := checkExists(env, path); err != nil {
		return nil, fmt.Errorf("repo.OpenWithEnv: %w", err)
	}

	d, err := db.Open(path)
	if err != nil {
		return nil, fmt.Errorf("repo.OpenWithEnv: %w", err)
	}

	id, err := d.ApplicationID()
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("repo.OpenWithEnv application_id: %w", err)
	}
	if id != libfossil.FossilApplicationID {
		d.Close()
		return nil, fmt.Errorf("repo.OpenWithEnv: not a fossil repo (application_id=%d, want %d)",
			id, libfossil.FossilApplicationID)
	}

	return &Repo{db: d, path: path}, nil
}

func (r *Repo) Close() error {
	return r.db.Close()
}

// Checkpoint runs PRAGMA wal_checkpoint(<mode>) against the repo's database.
// See db.DB.Checkpoint for semantics.
func (r *Repo) Checkpoint(mode db.CheckpointMode) error {
	return r.db.Checkpoint(mode)
}

func (r *Repo) Path() string {
	return r.path
}

func (r *Repo) DB() *db.DB {
	return r.db
}

func (r *Repo) WithTx(fn func(tx *db.Tx) error) error {
	return r.db.WithTx(fn)
}

// Config reads a configuration value from the repo's config table.
func (r *Repo) Config(key string) (string, error) {
	var value string
	err := r.db.QueryRow("SELECT value FROM config WHERE name=?", key).Scan(&value)
	if err != nil {
		return "", fmt.Errorf("repo.Config %q: %w", key, err)
	}
	return value, nil
}

// UUIDByRID returns the UUID (manifest hash) of the artifact with the given
// rid by querying the blob table. Returns sql.ErrNoRows (wrapped) when no
// blob row exists for the rid.
func (r *Repo) UUIDByRID(rid int64) (string, error) {
	var uuid string
	err := r.db.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid)
	if err != nil {
		return "", fmt.Errorf("repo.UUIDByRID rid=%d: %w", rid, err)
	}
	return uuid, nil
}

// SetConfig writes a configuration value to the repo's config table.
func (r *Repo) SetConfig(key, value string) error {
	_, err := r.db.Exec(
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES(?, ?, julianday('now'))",
		key, value,
	)
	if err != nil {
		return fmt.Errorf("repo.SetConfig %q: %w", key, err)
	}
	return nil
}

func (r *Repo) Verify() error {
	if r == nil {
		panic("repo.Verify: receiver must not be nil")
	}
	rows, err := r.db.Query("SELECT rid FROM blob WHERE size >= 0")
	if err != nil {
		return fmt.Errorf("repo.Verify query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return fmt.Errorf("repo.Verify scan: %w", err)
		}
		if err := content.Verify(r.db, libfossil.FslID(rid)); err != nil {
			return err
		}
	}
	return rows.Err()
}

