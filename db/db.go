package db

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

// CheckpointMode mirrors SQLite's PRAGMA wal_checkpoint(<mode>) argument.
type CheckpointMode int

const (
	// CheckpointPassive checkpoints frames not held by readers; never blocks.
	// Appropriate for periodic background checkpoints.
	CheckpointPassive CheckpointMode = iota
	// CheckpointFull blocks new writers until all frames are checkpointed.
	CheckpointFull
	// CheckpointRestart is FULL plus restarts the WAL file.
	CheckpointRestart
	// CheckpointTruncate is RESTART plus truncates the WAL file to zero bytes.
	// Produces the most compact on-disk file and is what Close uses.
	CheckpointTruncate
)

func (m CheckpointMode) String() string {
	switch m {
	case CheckpointPassive:
		return "PASSIVE"
	case CheckpointFull:
		return "FULL"
	case CheckpointRestart:
		return "RESTART"
	case CheckpointTruncate:
		return "TRUNCATE"
	default:
		return fmt.Sprintf("CheckpointMode(%d)", int(m))
	}
}

// DB wraps a SQLite database connection.
type DB struct {
	conn   *sql.DB
	path   string
	driver string
}

// Open opens a SQLite database with the registered driver and default pragmas.
func Open(path string) (*DB, error) {
	return OpenWith(path, OpenConfig{})
}

// OpenSQL opens a raw *sql.DB using the registered driver and the same DSN
// construction as OpenWith. params are appended as URI query parameters after
// driver pragmas, so callers can request options such as mode=ro without
// constructing driver-specific DSNs.
func OpenSQL(path string, cfg OpenConfig, params map[string]string) (*sql.DB, error) {
	driver, dsn := SQLDriverAndDSN(path, cfg, params)
	conn, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("db.OpenSQL(%s): %w", driver, err)
	}
	if err := applyWASMPragmas(conn, mergedPragmas(cfg.Pragmas)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db.OpenSQL: %w", err)
	}
	return conn, nil
}

// SQLDriverAndDSN returns the driver name and DSN OpenWith would use, with
// optional URI query params appended after driver-specific options.
func SQLDriverAndDSN(path string, cfg OpenConfig, params map[string]string) (string, string) {
	if path == "" {
		panic("db.SQLDriverAndDSN: path must not be empty")
	}
	if registered == nil {
		panic("db.SQLDriverAndDSN: no driver registered — import a driver package (e.g., _ \"github.com/danmestas/libfossil/db/driver/modernc\")")
	}
	driver := cfg.Driver
	if driver == "" {
		driver = registered.Name
	}
	pragmas := mergedPragmas(cfg.Pragmas)
	dsn := buildSQLDSN(path, pragmas)
	return driver, appendURIParams(dsn, params)
}

func mergedPragmas(overrides map[string]string) map[string]string {
	pragmas := DefaultPragmas()
	for k, v := range overrides {
		pragmas[k] = v
	}
	return pragmas
}

func buildSQLDSN(path string, pragmas map[string]string) string {
	if wasmClearPragmas {
		suffix := wasmDSNSuffix()
		if suffix != "" {
			return fmt.Sprintf("file:%s?%s", path, suffix)
		}
		return path
	}
	return appendURIParamString(registered.BuildDSN(path, pragmas), wasmDSNSuffix())
}

func appendURIParamString(dsn, params string) string {
	if params == "" {
		return dsn
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&" + params
	}
	if strings.HasPrefix(dsn, "file:") {
		return dsn + "?" + params
	}
	return "file:" + dsn + "?" + params
}

func appendURIParams(dsn string, params map[string]string) string {
	if len(params) == 0 {
		return dsn
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	if strings.Contains(dsn, "?") {
		b.WriteString(dsn)
		b.WriteByte('&')
	} else {
		if strings.HasPrefix(dsn, "file:") {
			b.WriteString(dsn)
		} else {
			b.WriteString("file:")
			b.WriteString(dsn)
		}
		b.WriteByte('?')
	}
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(params[k]))
	}
	return b.String()
}

func applyWASMPragmas(conn *sql.DB, pragmas map[string]string) error {
	if !wasmClearPragmas {
		return nil
	}
	// Apply safe pragmas via SQL. Skip journal_mode (WAL not supported on WASI).
	// Only apply known-safe pragmas to prevent SQL injection.
	safePragmas := map[string]bool{
		"busy_timeout": true,
		"foreign_keys": true,
	}
	for k, v := range pragmas {
		if !safePragmas[k] {
			continue
		}
		if _, err := conn.Exec(fmt.Sprintf("PRAGMA %s = %s", k, v)); err != nil {
			return fmt.Errorf("PRAGMA %s: %w", k, err)
		}
	}
	return nil
}

// OpenWith opens a SQLite database with explicit configuration.
func OpenWith(path string, cfg OpenConfig) (*DB, error) {
	driver, dsn := SQLDriverAndDSN(path, cfg, nil)
	pragmas := mergedPragmas(cfg.Pragmas)

	// WASM targets: skip DSN pragmas (ncruces _pragma syntax fails under WASI
	// file locking). Open with nolock=1, then apply safe pragmas via SQL.
	conn, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("db.Open(%s): %w", driver, err)
	}

	if err := applyWASMPragmas(conn, pragmas); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db.Open %w", err)
	}

	return &DB{conn: conn, path: path, driver: driver}, nil
}

// SqlDB returns the underlying *sql.DB connection.
func (d *DB) SqlDB() *sql.DB {
	if d == nil {
		panic("db.SqlDB: receiver must not be nil")
	}
	return d.conn
}

// Close runs PRAGMA wal_checkpoint(TRUNCATE) before closing the underlying
// connection so the on-disk file is readable by external SQLite/fossil
// tooling. The WASM/WASI build path does not use WAL, so the checkpoint is
// skipped there. Checkpoint errors are joined with the close error so the
// connection is always closed regardless of checkpoint outcome.
func (d *DB) Close() error {
	var ckptErr error
	if !wasmClearPragmas {
		ckptErr = d.Checkpoint(CheckpointTruncate)
	}
	closeErr := d.conn.Close()
	return errors.Join(ckptErr, closeErr)
}

// Checkpoint runs PRAGMA wal_checkpoint(<mode>) against the database.
// Safe to call on a live database. PASSIVE never blocks; TRUNCATE produces
// the most compact on-disk layout. On a non-WAL database the underlying
// PRAGMA is a no-op and returns nil.
func (d *DB) Checkpoint(mode CheckpointMode) error {
	if d == nil {
		panic("db.Checkpoint: receiver must not be nil")
	}
	if mode < CheckpointPassive || mode > CheckpointTruncate {
		return fmt.Errorf("db.Checkpoint: invalid mode %d", int(mode))
	}
	var busy, log, ckpt int
	err := d.conn.QueryRow(fmt.Sprintf("PRAGMA wal_checkpoint(%s)", mode)).Scan(&busy, &log, &ckpt)
	if err != nil {
		return fmt.Errorf("db.Checkpoint(%s): %w", mode, err)
	}
	return nil
}

func (d *DB) Path() string {
	return d.path
}

func (d *DB) Driver() string {
	return d.driver
}

func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	return d.conn.Exec(query, args...)
}

func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.conn.QueryRow(query, args...)
}

func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.conn.Query(query, args...)
}

func (d *DB) SetApplicationID(id int32) error {
	_, err := d.conn.Exec(fmt.Sprintf("PRAGMA application_id=%d", id))
	return err
}

func (d *DB) ApplicationID() (int32, error) {
	var id int32
	err := d.conn.QueryRow("PRAGMA application_id").Scan(&id)
	return id, err
}

type Tx struct {
	tx *sql.Tx
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.Exec(query, args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRow(query, args...)
}

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(query, args...)
}

func (d *DB) WithTx(fn func(tx *Tx) error) error {
	if d == nil {
		panic("db.WithTx: receiver must not be nil")
	}
	if fn == nil {
		panic("db.WithTx: fn must not be nil")
	}
	sqlTx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("db.WithTx begin: %w", err)
	}
	defer func() {
		if rbErr := sqlTx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			fmt.Fprintf(os.Stderr, "db.WithTx: rollback failed: %v\n", rbErr)
		}
	}()
	if err := fn(&Tx{tx: sqlTx}); err != nil {
		return err
	}
	return sqlTx.Commit()
}
