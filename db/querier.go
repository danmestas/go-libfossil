package db

import (
	"database/sql"
	"strings"
	"sync"
)

// Querier is the common interface satisfied by both *DB and *Tx.
// Functions that need to work inside transactions accept Querier
// instead of *DB.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

// preparer is the subset of *sql.DB and *sql.Tx used to compile statements.
// Both expose an identical Prepare, letting a single stmtCache back either a
// connection pool or a transaction.
type preparer interface {
	Prepare(query string) (*sql.Stmt, error)
}

// stmtCache memoizes prepared statements by exact query text so a query
// executed repeatedly with the same text is compiled once instead of on every
// call. One cache is owned by a single *DB (backed by its *sql.DB pool) or a
// single *Tx (backed by its *sql.Tx); it is never shared across transactions.
// The zero value is not usable — set src before first use. Safe for concurrent
// use by multiple goroutines, matching *sql.DB's own contract.
//
// The *DB-backed cache has no eviction: every distinct query text a caller
// ever runs through it stays cached for the pool's lifetime. This is fine for
// this codebase's fixed, bounded set of query shapes, but a caller that builds
// query text by interpolating a value (a UUID, a table name) instead of
// binding it as a placeholder arg would grow the cache without bound. Always
// parameterize with `?` — never inline a value into the query string.
type stmtCache struct {
	src   preparer
	mu    sync.Mutex
	stmts map[string]*sql.Stmt
}

// get returns the cached statement for query, compiling and caching it on
// first use. Preparation happens outside the lock so a slow compile never
// blocks lookups of already-cached statements; a lost race simply discards the
// redundant handle.
func (c *stmtCache) get(query string) (*sql.Stmt, error) {
	c.mu.Lock()
	if st, ok := c.stmts[query]; ok {
		c.mu.Unlock()
		return st, nil
	}
	c.mu.Unlock()

	st, err := c.src.Prepare(query)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.stmts[query]; ok {
		// Another goroutine won the race; keep the first handle and drop ours.
		_ = st.Close()
		return existing, nil
	}
	if c.stmts == nil {
		c.stmts = make(map[string]*sql.Stmt)
	}
	c.stmts[query] = st
	return st, nil
}

// closeAll closes every cached statement and empties the cache. It is safe to
// call more than once and after the backing pool or transaction has closed:
// *sql.Stmt.Close is idempotent.
func (c *stmtCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, st := range c.stmts {
		_ = st.Close()
	}
	c.stmts = nil
}

// size reports the number of distinct statements currently cached. Used by
// tests to assert that repeated identical queries prepare only once.
func (c *stmtCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.stmts)
}

// cacheableStmt reports whether query is a single statement eligible for a
// cached prepared statement. A prepared statement compiles exactly one SQL
// statement, so multi-statement strings (e.g. DDL setup batches that create a
// table and its indexes in one call) must run through the raw driver path that
// splits and executes each statement. A semicolon anywhere other than trailing
// whitespace signals more than one statement. A false negative — a semicolon
// inside a string literal — only forgoes caching for that query and never
// changes results.
func cacheableStmt(query string) bool {
	return !strings.Contains(strings.TrimRight(query, " \t\r\n;"), ";")
}
