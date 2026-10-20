package db

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
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
	conn      *sql.DB
	path      string
	driver    string
	stmts     stmtCache
	poolExecs atomic.Int64
	writeTxns atomic.Int64
	// root is set on a DB that WithBoundTx hands its callback: the pool's own
	// DB, which keeps the counters. bound is the transaction every statement
	// on it runs in, nil once that transaction has ended.
	root  *DB
	bound *Tx
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
		panic("db.SQLDriverAndDSN: no driver registered — import a driver package (e.g., _ \"github.com/danmestas/go-libfossil/db/driver/modernc\")")
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
	return appendURIParamString(orderPragmas(registered.BuildDSN(path, pragmas)), wasmDSNSuffix())
}

// pragmaOrder is the order a DSN's pragmas run in, ahead of any other pragma,
// which follow by name. busy_timeout is first: a pragma run before it fails
// at once with "database is locked" while another process holds a lock,
// instead of waiting. journal_mode is next because it needs the write lock,
// and the rest are connection settings that touch no file.
var pragmaOrder = []string{"busy_timeout", "journal_mode", "locking_mode", "synchronous", "foreign_keys"}

// pragmaRank is name's place in pragmaOrder, or len(pragmaOrder) for a
// pragma that is not in it.
func pragmaRank(name string) int {
	if i := slices.Index(pragmaOrder, name); i >= 0 {
		return i
	}
	return len(pragmaOrder)
}

// orderPragmas puts the DSN's _pragma parameters in pragmaOrder, then by
// name, leaving every other parameter where it was. The drivers run the
// pragmas in DSN order and build the DSN from a map, so without this the
// order varied by open.
func orderPragmas(dsn string) string {
	base, query, ok := strings.Cut(dsn, "?")
	if !ok {
		return dsn
	}
	params := strings.Split(query, "&")
	var slots []int
	var pragmas []string
	for i, p := range params {
		if strings.HasPrefix(p, "_pragma=") {
			slots = append(slots, i)
			pragmas = append(pragmas, p)
		}
	}
	name := func(p string) string {
		n, _, _ := strings.Cut(strings.TrimPrefix(p, "_pragma="), "(")
		return n
	}
	slices.SortFunc(pragmas, func(a, b string) int {
		if c := cmp.Compare(pragmaRank(name(a)), pragmaRank(name(b))); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	for i, slot := range slots {
		params[slot] = pragmas[i]
	}
	return base + "?" + strings.Join(params, "&")
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
	// Apply safe pragmas via SQL, in pragmaOrder. Skip journal_mode (WAL not
	// supported on WASI). Only apply known-safe pragmas to prevent SQL
	// injection.
	for _, k := range []string{"busy_timeout", "foreign_keys"} {
		v, ok := pragmas[k]
		if !ok {
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

	return &DB{conn: conn, path: path, driver: driver, stmts: stmtCache{src: conn}}, nil
}

// SqlDB returns the underlying *sql.DB connection.
func (d *DB) SqlDB() *sql.DB {
	if d == nil {
		panic("db.SqlDB: receiver must not be nil")
	}
	d.mustBePool("SqlDB")
	return d.conn
}

// Close runs PRAGMA wal_checkpoint(TRUNCATE) before closing the underlying
// connection so the on-disk file is readable by external SQLite/fossil
// tooling. The WASM/WASI build path does not use WAL, so the checkpoint is
// skipped there. Checkpoint errors are joined with the close error so the
// connection is always closed regardless of checkpoint outcome.
func (d *DB) Close() error {
	d.mustBePool("Close")
	var ckptErr error
	if !wasmClearPragmas {
		ckptErr = d.Checkpoint(CheckpointTruncate)
	}
	// Release cached prepared statements before closing the pool so no
	// statement handle outlives the connection it was compiled against.
	d.stmts.closeAll()
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
	d.mustBePool("Checkpoint")
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

// PoolExecs returns how many statements have been executed on the pool rather
// than inside a transaction. Each one runs in its own implicit transaction, so
// it costs a BEGIN IMMEDIATE and an fsync of its own and contests the database's
// single write lock independently of every other. A path that issues one per
// artifact therefore degrades from slow to failing the moment anything else
// writes, which is what issue #200 cost a large pull. Tests pin the per-round
// budget against this counter.
func (d *DB) PoolExecs() int64 {
	if d == nil {
		panic("db.PoolExecs: receiver must not be nil")
	}
	if d.root != nil {
		return d.root.PoolExecs()
	}
	return d.poolExecs.Load()
}

// WriteTxns counts transactions begun through WithTx. Each is its own
// BEGIN IMMEDIATE and fsync, and each takes the single write lock
// independently, so a path that opens one per artifact is both slow and
// fragile under contention -- the cost issue #200 measured at roughly three
// per received file. Tests pin the per-round transaction budget against this,
// which PoolExecs cannot see: moving a per-artifact write from the pool into
// its own transaction leaves PoolExecs unchanged while the real cost stays.
func (d *DB) WriteTxns() int64 {
	if d == nil {
		panic("db.WriteTxns: receiver must not be nil")
	}
	if d.root != nil {
		return d.root.WriteTxns()
	}
	return d.writeTxns.Load()
}

func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	if tx := d.boundTx(); tx != nil {
		return tx.Exec(query, args...)
	}
	d.poolExecs.Add(1)
	if !cacheableStmt(query) {
		return d.conn.Exec(query, args...)
	}
	st, err := d.stmts.get(query)
	if err != nil {
		return nil, err
	}
	return st.Exec(args...)
}

func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	if tx := d.boundTx(); tx != nil {
		return tx.QueryRow(query, args...)
	}
	if !cacheableStmt(query) {
		return d.conn.QueryRow(query, args...)
	}
	st, err := d.stmts.get(query)
	if err != nil {
		// Preserve database/sql's deferred-error contract: a prepare failure
		// must surface from Row.Scan, never here. The raw QueryRow re-attempts
		// the prepare internally and returns a *sql.Row that yields the error
		// on Scan — identical to the pre-cache behavior.
		return d.conn.QueryRow(query, args...)
	}
	return st.QueryRow(args...)
}

func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	if tx := d.boundTx(); tx != nil {
		return tx.Query(query, args...)
	}
	if !cacheableStmt(query) {
		return d.conn.Query(query, args...)
	}
	st, err := d.stmts.get(query)
	if err != nil {
		return nil, err
	}
	return st.Query(args...)
}

func (d *DB) SetApplicationID(id int32) error {
	if tx := d.boundTx(); tx != nil {
		_, err := tx.tx.Exec(fmt.Sprintf("PRAGMA application_id=%d", id))
		return err
	}
	_, err := d.conn.Exec(fmt.Sprintf("PRAGMA application_id=%d", id))
	return err
}

func (d *DB) ApplicationID() (int32, error) {
	var id int32
	if tx := d.boundTx(); tx != nil {
		err := tx.tx.QueryRow("PRAGMA application_id").Scan(&id)
		return id, err
	}
	err := d.conn.QueryRow("PRAGMA application_id").Scan(&id)
	return id, err
}

type Tx struct {
	tx    *sql.Tx
	stmts stmtCache
	// savepoints is how many savepoints a bound DB's nested WithTx has open
	// in this transaction; it names the next one.
	savepoints int
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	if !cacheableStmt(query) {
		return t.tx.Exec(query, args...)
	}
	st, err := t.stmts.get(query)
	if err != nil {
		return nil, err
	}
	return st.Exec(args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	if !cacheableStmt(query) {
		return t.tx.QueryRow(query, args...)
	}
	st, err := t.stmts.get(query)
	if err != nil {
		// Preserve the deferred-error contract; see DB.QueryRow.
		return t.tx.QueryRow(query, args...)
	}
	return st.QueryRow(args...)
}

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	if !cacheableStmt(query) {
		return t.tx.Query(query, args...)
	}
	st, err := t.stmts.get(query)
	if err != nil {
		return nil, err
	}
	return st.Query(args...)
}

// WithTx runs fn in a write transaction, which commits when fn returns nil
// and rolls back otherwise. On a DB that WithBoundTx bound, it runs fn in a
// savepoint of the bound transaction instead, so it neither waits for a
// second connection nor commits on its own: fn's error undoes only fn's
// writes, and the bound transaction decides whether the rest commits.
func (d *DB) WithTx(fn func(tx *Tx) error) error {
	if d == nil {
		panic("db.WithTx: receiver must not be nil")
	}
	if fn == nil {
		panic("db.WithTx: fn must not be nil")
	}
	if tx := d.boundTx(); tx != nil {
		return tx.withSavepoint(fn)
	}
	d.writeTxns.Add(1)
	sqlTx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("db.WithTx begin: %w", err)
	}
	tx := &Tx{tx: sqlTx, stmts: stmtCache{src: sqlTx}}
	defer func() {
		// A statement prepared against this transaction cannot outlive it, so
		// close the whole cache before the transaction ends. This runs on
		// every path (commit, error, panic); Close is idempotent, so closing
		// again after a successful Commit is a safe no-op.
		tx.stmts.closeAll()
		if rbErr := sqlTx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			fmt.Fprintf(os.Stderr, "db.WithTx: rollback failed: %v\n", rbErr)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return sqlTx.Commit()
}

// WithBoundTx runs fn in one write transaction and hands it a DB bound to that
// transaction. Every statement on the bound DB runs in the transaction, and
// every WithTx or WithBoundTx on it is a savepoint there, so code written
// against a *DB, including code that opens its own transactions, joins this
// one instead of contending for the write lock from a second connection. The
// transaction commits when fn returns nil and rolls back everything fn did
// otherwise. The bound DB is valid only until fn returns, and it cannot be
// closed or checkpointed, nor hand out its *sql.DB. On a DB that is itself
// bound, WithBoundTx is a savepoint of that transaction, as WithTx is.
func (d *DB) WithBoundTx(fn func(bound *DB) error) error {
	if d == nil {
		panic("db.WithBoundTx: receiver must not be nil")
	}
	if fn == nil {
		panic("db.WithBoundTx: fn must not be nil")
	}
	if d.boundTx() != nil {
		return d.WithTx(func(*Tx) error { return fn(d) })
	}
	return d.WithTx(func(tx *Tx) error {
		b := &DB{path: d.path, driver: d.driver, root: d, bound: tx}
		// Unbind on every path, so a bound DB kept past fn fails loudly
		// rather than running on a finished transaction.
		defer func() { b.bound = nil }()
		return fn(b)
	})
}

// boundTx returns the transaction a bound DB runs in, nil for the pool's DB.
// A bound DB used after its transaction ended is a programmer error.
func (d *DB) boundTx() *Tx {
	if d.root != nil && d.bound == nil {
		panic("db: a bound DB was used after its transaction ended")
	}
	return d.bound
}

// mustBePool panics when op is called on a bound DB, which does not own the
// pool it would act on.
func (d *DB) mustBePool(op string) {
	if d.root != nil {
		panic("db." + op + ": not allowed on a DB bound to a transaction")
	}
}

// withSavepoint runs fn in a savepoint of t. fn's error rolls back to the
// savepoint and is returned, leaving t open for the writes before it.
func (t *Tx) withSavepoint(fn func(tx *Tx) error) (err error) {
	t.savepoints++
	name := fmt.Sprintf("withtx_%d", t.savepoints)
	defer func() { t.savepoints-- }()
	if _, err := t.tx.Exec("SAVEPOINT " + name); err != nil {
		return fmt.Errorf("db.WithTx savepoint: %w", err)
	}
	done := false
	defer func() {
		if done {
			return
		}
		// fn failed or panicked: undo its writes. A failure here means
		// SQLite already ended the whole transaction, which its commit
		// will then report.
		_, rbErr := t.tx.Exec("ROLLBACK TO " + name)
		_, relErr := t.tx.Exec("RELEASE " + name)
		if rbErr != nil || relErr != nil {
			err = errors.Join(err, rbErr, relErr)
		}
	}()
	if err := fn(t); err != nil {
		return err
	}
	if _, err := t.tx.Exec("RELEASE " + name); err != nil {
		return fmt.Errorf("db.WithTx release: %w", err)
	}
	done = true
	return nil
}
