package modernc

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
)

// TestBuildDSN_TxlockImmediate verifies the registered DSN builder always
// emits _txlock=immediate, regardless of how many pragmas are merged in.
// This is the load-bearing piece that prevents the SHARED→RESERVED upgrade
// race documented in issue #33.
func TestBuildDSN_TxlockImmediate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pragmas map[string]string
	}{
		{"no pragmas", nil},
		{"empty pragmas", map[string]string{}},
		{"with pragmas", map[string]string{"journal_mode": "WAL", "busy_timeout": "5000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dsn := buildDSN("/tmp/x.db", tc.pragmas)
			if !strings.Contains(dsn, "_txlock=immediate") {
				t.Fatalf("buildDSN missing _txlock=immediate: %q", dsn)
			}
		})
	}
}

// TestConcurrentWritersDoNotRace exercises the failure mode from issue #33:
// two writers on the same SQLite file, each doing SELECT-then-INSERT inside
// a transaction. With the default DEFERRED transaction mode under WAL,
// SQLite returns SQLITE_BUSY immediately on the SHARED→RESERVED upgrade
// (bypassing busy_timeout) ~50% of the time. With _txlock=immediate set in
// the DSN, BEGIN itself acquires RESERVED and the second writer blocks at
// BEGIN where busy_timeout applies — so both transactions serialize and
// neither fails.
//
// Asserts that the registered driver's DSN produces zero SQLITE_BUSY
// failures across N iterations of contended concurrent commits.
func TestConcurrentWritersDoNotRace(t *testing.T) {
	t.Parallel()

	const iterations = 25
	path := filepath.Join(t.TempDir(), "concurrent.db")
	dsn := buildDSN(path, db.DefaultPragmas())

	dbA, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer dbA.Close()
	dbB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer dbB.Close()

	if _, err := dbA.Exec("CREATE TABLE t(x INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	var ok, fail int
	var failErrs []error
	for range iterations {
		var wg sync.WaitGroup
		wg.Add(2)
		errs := make(chan error, 2)
		begin := make(chan struct{})

		work := func(d *sql.DB, val int) {
			defer wg.Done()
			<-begin
			tx, err := d.BeginTx(context.Background(), nil)
			if err != nil {
				errs <- err
				return
			}
			var n int
			if err := tx.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
				_ = tx.Rollback()
				errs <- err
				return
			}
			// Widen the upgrade window to make the failure mode reliably
			// reproducible on the deferred-default DSN. With _txlock=immediate
			// this sleep is irrelevant — both BEGINs already hold (or wait on)
			// the write lock.
			time.Sleep(5 * time.Millisecond)
			if _, err := tx.Exec("INSERT INTO t VALUES (?)", val); err != nil {
				_ = tx.Rollback()
				errs <- err
				return
			}
			errs <- tx.Commit()
		}

		go work(dbA, 1)
		go work(dbB, 2)
		close(begin)
		wg.Wait()
		close(errs)

		for e := range errs {
			if e == nil {
				ok++
			} else {
				fail++
				failErrs = append(failErrs, e)
			}
		}
	}

	if fail != 0 {
		// Surface a sample so a regression is diagnosable from CI logs.
		var sample error
		if len(failErrs) > 0 {
			sample = failErrs[0]
		}
		t.Fatalf("expected zero SQLITE_BUSY across %d iterations, got fail=%d ok=%d; first error: %v",
			iterations, fail, ok, sample)
	}
	if ok != 2*iterations {
		t.Fatalf("expected ok=%d, got %d", 2*iterations, ok)
	}
}
