package db

import (
	"path/filepath"
	"testing"
)

// newBlobDB opens a scratch database seeded with a single-row blob table that
// mirrors the shape the profiled hot paths query (SELECT content, size FROM
// blob WHERE rid=?).
func newBlobDB(t testing.TB) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "stmtcache.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := d.Exec("CREATE TABLE blob(rid INTEGER PRIMARY KEY, content BLOB, size INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := d.Exec("INSERT INTO blob(rid, content, size) VALUES (1, ?, ?)", []byte("hello"), int64(5)); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	return d
}

// TestDBStatementCacheReusesPreparedStatement is the core regression test for
// issue #170: repeatedly executing the identical query text must compile a
// single prepared statement and reuse it, not re-prepare on every call. It
// asserts the observable cache size stays at exactly one distinct statement
// across N invocations. Against the pre-fix code (which called the raw
// connection every time and cached nothing) this reuse did not happen; the
// SQLite parser/compiler ran once per call.
func TestDBStatementCacheReusesPreparedStatement(t *testing.T) {
	d := newBlobDB(t)
	defer d.Close()

	const q = "SELECT content, size FROM blob WHERE rid=?"
	const n = 100
	base := d.stmts.size() // statements cached by table setup
	for i := 0; i < n; i++ {
		var content []byte
		var size int64
		if err := d.QueryRow(q, 1).Scan(&content, &size); err != nil {
			t.Fatalf("QueryRow[%d]: %v", i, err)
		}
		if size != 5 {
			t.Fatalf("QueryRow[%d]: size=%d, want 5", i, size)
		}
	}

	if got := d.stmts.size(); got != base+1 {
		t.Fatalf("after %d identical queries, cache grew by %d statements, want 1", n, got-base)
	}

	// The cache must hand back the same *sql.Stmt on every lookup.
	st1, err := d.stmts.get(q)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	st2, err := d.stmts.get(q)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st1 != st2 {
		t.Fatal("cache returned distinct statements for identical query text")
	}

	// A distinct query text prepares (and caches) a separate statement.
	if _, err := d.Query("SELECT rid FROM blob WHERE size=?", 5); err != nil {
		t.Fatalf("second query: %v", err)
	}
	if got := d.stmts.size(); got != base+2 {
		t.Fatalf("after a second distinct query, cache grew by %d statements, want 2", got-base)
	}
}

// TestMultiStatementBypassesCache confirms that multi-statement SQL (e.g. DDL
// setup batches) is executed through the raw driver path and never cached — a
// prepared statement only compiles one statement, so caching such a string
// would silently drop everything after the first semicolon.
func TestMultiStatementBypassesCache(t *testing.T) {
	d := newBlobDB(t)
	defer d.Close()

	before := d.stmts.size()
	if _, err := d.Exec("CREATE TABLE a(x INTEGER); CREATE TABLE b(y INTEGER);"); err != nil {
		t.Fatalf("multi-statement Exec: %v", err)
	}
	if got := d.stmts.size(); got != before {
		t.Fatalf("multi-statement Exec changed cache size from %d to %d; it must not be cached", before, got)
	}
	// Both statements must have executed.
	for _, tbl := range []string{"a", "b"} {
		var name string
		if err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name); err != nil {
			t.Fatalf("table %q not created: %v", tbl, err)
		}
	}
}

// TestQueryRowPrepareFailureDefersError verifies the database/sql contract that
// a QueryRow whose statement fails to prepare surfaces the error from Scan
// rather than panicking or losing it.
func TestQueryRowPrepareFailureDefersError(t *testing.T) {
	d := newBlobDB(t)
	defer d.Close()

	before := d.stmts.size()
	var v int
	err := d.QueryRow("SELECT this is not valid sql").Scan(&v)
	if err == nil {
		t.Fatal("expected Scan to return the deferred prepare error, got nil")
	}
	// The bad query must not have been cached.
	if got := d.stmts.size(); got != before {
		t.Fatalf("failed-prepare query changed cache size from %d to %d, want unchanged", before, got)
	}
}

// TestDBCloseClosesCachedStatements confirms Close releases every cached
// statement handle so nothing leaks across the lifetime of a DB.
func TestDBCloseClosesCachedStatements(t *testing.T) {
	d := newBlobDB(t)

	const q = "SELECT content, size FROM blob WHERE rid=?"
	st, err := d.stmts.get(q)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := d.stmts.size(); got != 0 {
		t.Fatalf("cache holds %d statements after Close, want 0", got)
	}
	// A statement closed by Close must reject further use.
	if _, err := st.Query(1); err == nil {
		t.Fatal("expected error using a statement after DB.Close, got nil")
	}
}

// TestTxStatementCacheReuseAndCleanup verifies that a transaction reuses a
// prepared statement across repeated identical queries within its own lifetime
// and that the cache is torn down (statements closed) when the transaction
// ends — no statement is reachable or reused after commit.
func TestTxStatementCacheReuseAndCleanup(t *testing.T) {
	d := newBlobDB(t)
	defer d.Close()

	const q = "SELECT content, size FROM blob WHERE rid=?"
	var captured *Tx
	err := d.WithTx(func(tx *Tx) error {
		for i := 0; i < 50; i++ {
			var content []byte
			var size int64
			if err := tx.QueryRow(q, 1).Scan(&content, &size); err != nil {
				return err
			}
		}
		if got := tx.stmts.size(); got != 1 {
			t.Fatalf("in-transaction cache holds %d statements, want 1", got)
		}
		captured = tx
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	// After the transaction ends the cache is emptied.
	if got := captured.stmts.size(); got != 0 {
		t.Fatalf("tx cache holds %d statements after commit, want 0", got)
	}
}

// TestManyShortTransactionsStayHealthy opens many short-lived transactions
// that each run the same query text and confirms the connection pool stays
// bounded across the run. This is a smoke test for overall pool health under
// many tx cycles, not a leak-detection mechanism in its own right: the pool
// reuses connections regardless of whether a transaction's cached statements
// were actually closed, so this assertion alone would still pass against a
// no-op closeAll. The actual guarantee that a transaction's cache is torn
// down on exit is TestTxStatementCacheReuseAndCleanup, which asserts the
// cache is empty immediately after commit.
func TestManyShortTransactionsStayHealthy(t *testing.T) {
	d := newBlobDB(t)
	defer d.Close()

	const q = "SELECT content, size FROM blob WHERE rid=?"
	for i := 0; i < 200; i++ {
		err := d.WithTx(func(tx *Tx) error {
			var content []byte
			var size int64
			return tx.QueryRow(q, 1).Scan(&content, &size)
		})
		if err != nil {
			t.Fatalf("WithTx[%d]: %v", i, err)
		}
	}
	if open := d.SqlDB().Stats().OpenConnections; open > 4 {
		t.Fatalf("open connections grew to %d after 200 short transactions; suspected statement leak", open)
	}
}

// benchQuery is the exact hot-path lookup from internal/blob's blob loader.
const benchQuery = "SELECT content, size FROM blob WHERE rid=?"

// BenchmarkQueryRowCached measures the fixed path: the prepared statement is
// compiled once and reused on every call.
func BenchmarkQueryRowCached(b *testing.B) {
	d := newBlobDB(b)
	defer d.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var content []byte
		var size int64
		if err := d.QueryRow(benchQuery, 1).Scan(&content, &size); err != nil {
			b.Fatalf("scan: %v", err)
		}
	}
}

// BenchmarkQueryRowUncached measures the pre-fix path: going straight to the
// pool re-compiles the query text on every call. Run alongside
// BenchmarkQueryRowCached to see the reuse win.
func BenchmarkQueryRowUncached(b *testing.B) {
	d := newBlobDB(b)
	defer d.Close()
	raw := d.SqlDB()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var content []byte
		var size int64
		if err := raw.QueryRow(benchQuery, 1).Scan(&content, &size); err != nil {
			b.Fatalf("scan: %v", err)
		}
	}
}
