package repo

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/simio"
)

type Repo struct {
	db   *db.DB
	path string
	// receipt is what content stored through this Repo is recorded under;
	// nil leaves each transaction its own (see WithReceipt).
	receipt *db.Receipt
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

	if err := db.SeedConfig(d, env.Rand, projectCode); err != nil {
		d.Close()
		if rmErr := env.Storage.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("repo.CreateWithEnv: %w (cleanup failed: %v)", err, rmErr)
		}
		return nil, fmt.Errorf("repo.CreateWithEnv seed config: %w", err)
	}

	// After SeedConfig, so a seeded Rand still yields the project and server
	// codes it did before these users were added.
	if err := db.SeedDefaultUsers(d, env.Rand); err != nil {
		d.Close()
		if rmErr := env.Storage.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("repo.CreateWithEnv: %w (cleanup failed: %v)", err, rmErr)
		}
		return nil, fmt.Errorf("repo.CreateWithEnv seed default users: %w", err)
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
	// The conversion is best-effort: while another connection holds the
	// write lock it is left for a later open, rather than failing this one.
	if err := upgradeJulianSeconds(d); err != nil && !isBusy(err) {
		d.Close()
		return nil, fmt.Errorf("repo.OpenWithEnv: %w", err)
	}

	return &Repo{db: d, path: path}, nil
}

// julianSecondsRows matches the config and user rows whose mtime is a Julian
// day, as go-libfossil wrote it before writing seconds since 1970 as fossil
// does. A Julian day now is about 2.46e6; as seconds that is January 1970,
// a time no row fossil or go-libfossil writes today holds.
const julianSecondsRows = "mtime BETWEEN 2000000 AND 3000000"

// isBusy reports whether err is SQLite's SQLITE_BUSY or SQLITE_LOCKED, as
// either driver words it.
func isBusy(err error) bool {
	msg := err.Error()
	for _, s := range []string{"database is locked", "database table is locked",
		"SQLITE_BUSY", "SQLITE_LOCKED"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// upgradeJulianSeconds converts those rows to the seconds they stand for.
// It reads first and writes only when a row needs it, so opening an
// up-to-date repository takes no write lock.
func upgradeJulianSeconds(d *db.DB) error {
	var n int
	err := d.QueryRow("SELECT (SELECT count(*) FROM config WHERE " + julianSecondsRows +
		") + (SELECT count(*) FROM user WHERE " + julianSecondsRows + ")").Scan(&n)
	if err != nil {
		return fmt.Errorf("check julian mtimes: %w", err)
	}
	if n == 0 {
		return nil
	}
	return d.WithTx(func(tx *db.Tx) error {
		for _, table := range []string{"config", "user"} {
			if _, err := tx.Exec("UPDATE " + table +
				" SET mtime=CAST((mtime-2440587.5)*86400 AS INTEGER) WHERE " +
				julianSecondsRows); err != nil {
				return fmt.Errorf("convert %s julian mtimes: %w", table, err)
			}
		}
		return nil
	})
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
	if r.receipt == nil {
		return r.db.WithTx(fn)
	}
	return r.db.WithTx(func(tx *db.Tx) error {
		prev := tx.UseReceipt(r.receipt)
		defer tx.UseReceipt(prev)
		return fn(tx)
	})
}

// WithReceipt returns a Repo on the same database that records the content
// its transactions store under rc: one receipt for a whole commit, sync,
// clone or served request, as fossil keeps one g.rcvid for its process. It is
// valid for as long as r is, and closing it closes r.
func (r *Repo) WithReceipt(rc *db.Receipt) *Repo {
	if rc == nil {
		panic("repo.WithReceipt: rc must not be nil")
	}
	return &Repo{db: r.db, path: r.path, receipt: rc}
}

// Receipt returns the receipt given by WithReceipt, nil when there is none.
func (r *Repo) Receipt() *db.Receipt {
	return r.receipt
}

// LocalUID returns the uid of login in the user table, the user content is
// credited to. A login the repository lacks falls back to its first setup
// user, as fossil's clone takes the user holding s when it has no other
// (src/clone.c); a repository with neither credits uid 0.
func (r *Repo) LocalUID(login string) (int64, error) {
	var uid int64
	err := r.db.QueryRow("SELECT coalesce("+
		"(SELECT uid FROM user WHERE login=?),"+
		"(SELECT uid FROM user WHERE cap LIKE '%s%' ORDER BY uid LIMIT 1), 0)",
		login).Scan(&uid)
	if err != nil {
		return 0, fmt.Errorf("repo.LocalUID %q: %w", login, err)
	}
	return uid, nil
}

// WithBoundTx runs fn in one write transaction with a Repo bound to it, as
// db.DB.WithBoundTx binds its DB: everything fn does through that Repo,
// including the transactions code running on it opens, commits together when
// fn returns nil and rolls back together otherwise. The bound Repo is valid
// only until fn returns and must not be closed.
func (r *Repo) WithBoundTx(fn func(bound *Repo) error) error {
	if fn == nil {
		panic("repo.WithBoundTx: fn must not be nil")
	}
	return r.db.WithBoundTx(func(b *db.DB) error {
		if r.receipt != nil {
			prev := b.UseReceipt(r.receipt)
			defer b.UseReceipt(prev)
		}
		return fn(&Repo{db: b, path: r.path, receipt: r.receipt})
	})
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

// SetConfig writes a configuration value to the repo's config table, stamped
// with the time in seconds since 1970, as fossil's db_set stamps it (now()).
//
// A hash-policy value must be one of the ordinals 0 through 4, the only form
// the reader accepts (AF-7.1-2). Fossil makes an unknown policy fatal where
// the value comes in (hname.c hash_policy_command), so a bad value is refused
// here instead of poisoning every later store.
func (r *Repo) SetConfig(key, value string) error {
	if key == "hash-policy" {
		if n, err := strconv.Atoi(value); err != nil || n < 0 || n > 4 {
			return fmt.Errorf("repo.SetConfig: hash-policy %q is not a policy ordinal (0-4)", value)
		}
	}
	_, err := r.db.Exec(
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES(?, ?, strftime('%s','now'))",
		key, value,
	)
	if err != nil {
		return fmt.Errorf("repo.SetConfig %q: %w", key, err)
	}
	return nil
}

// SetHashPolicy assigns the hash policy by its value string, as fossil's
// hash-policy command does: name must be one of sha1, auto, sha3, sha3-only
// or shun-sha1, matched case-sensitively and exactly, and anything else is
// an error that leaves the stored policy alone (artifact-format-00 7.1,
// AF-7.1-2). The ordinal is what is stored, the form the reader takes.
//
// Asking for auto in a repo that already holds a SHA3 artifact stores sha3
// (7.3, set-time coercion): auto would be promoted by the first SHA3 store
// anyway, and fossil's command coerces the same way.
func (r *Repo) SetHashPolicy(name string) error {
	if r == nil {
		panic("repo.SetHashPolicy: receiver must not be nil")
	}
	policy, err := hash.PolicyByName(name)
	if err != nil {
		return fmt.Errorf("repo.SetHashPolicy: %w", err)
	}
	if policy != db.HashPolicyAuto {
		return r.SetConfig("hash-policy", strconv.Itoa(policy))
	}
	// The SHA3 check and the write are one statement, so a SHA3 artifact
	// stored concurrently cannot slip between them and have the write undo
	// the ratchet (7.3).
	_, err = r.db.Exec(
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES('hash-policy', "+
			"CASE WHEN EXISTS(SELECT 1 FROM blob WHERE length(uuid)>40) THEN ? ELSE ? END, "+
			"strftime('%s','now'))",
		strconv.Itoa(db.HashPolicySHA3), strconv.Itoa(db.HashPolicyAuto),
	)
	if err != nil {
		return fmt.Errorf("repo.SetHashPolicy: %w", err)
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
