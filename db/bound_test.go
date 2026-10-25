package db_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
)

// openBoundTestDB opens a database with one table t(v) for the bound
// transaction tests.
func openBoundTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "bound.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Exec("CREATE TABLE t(v INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return d
}

func countRows(t *testing.T, q db.Querier) int {
	t.Helper()
	var n int
	if err := q.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestWithBoundTxCommitsAndRollsBack(t *testing.T) {
	d := openBoundTestDB(t)
	err := d.WithBoundTx(func(b *db.DB) error {
		_, err := b.Exec("INSERT INTO t VALUES(1)")
		return err
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	boom := errors.New("boom")
	err = d.WithBoundTx(func(b *db.DB) error {
		if _, err := b.Exec("INSERT INTO t VALUES(2)"); err != nil {
			return err
		}
		// A nested WithTx that commits is still undone by the outer error.
		if err := b.WithTx(func(tx *db.Tx) error {
			_, err := tx.Exec("INSERT INTO t VALUES(3)")
			return err
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if n := countRows(t, d); n != 1 {
		t.Fatalf("rows after rollback = %d, want 1", n)
	}
}

// A nested WithTx joins the bound transaction as a savepoint: it sees the
// outer writes, its error undoes only its own, and it never waits on a
// second connection for the write lock the outer transaction holds.
func TestWithBoundTxNestedIsSavepoint(t *testing.T) {
	d := openBoundTestDB(t)
	before := d.WriteTxns()
	err := d.WithBoundTx(func(b *db.DB) error {
		if _, err := b.Exec("INSERT INTO t VALUES(1)"); err != nil {
			return err
		}
		inner := b.WithTx(func(tx *db.Tx) error {
			if n := countRows(t, tx); n != 1 {
				t.Errorf("nested tx sees %d rows, want 1", n)
			}
			if _, err := tx.Exec("INSERT INTO t VALUES(2)"); err != nil {
				return err
			}
			return errors.New("undo the nested write")
		})
		if inner == nil {
			t.Error("nested WithTx error was lost")
		}
		if n := countRows(t, b); n != 1 {
			t.Errorf("after nested rollback %d rows, want 1", n)
		}
		// Nesting a bound transaction in a bound one joins it too.
		return b.WithBoundTx(func(bb *db.DB) error {
			_, err := bb.Exec("INSERT INTO t VALUES(3)")
			return err
		})
	})
	if err != nil {
		t.Fatalf("WithBoundTx: %v", err)
	}
	if n := countRows(t, d); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	if got := d.WriteTxns() - before; got != 1 {
		t.Fatalf("write transactions = %d, want 1", got)
	}
}

// Writes counts a write once it has finished, so a reader that sees the count
// unchanged knows no write of this DB landed in between: a pool Exec counts
// when it returns, a transaction only when it ends, and a read counts nothing.
func TestWritesCountsFinishedWrites(t *testing.T) {
	d := openBoundTestDB(t)
	before := d.Writes()
	if _, err := d.Exec("INSERT INTO t VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	if got := d.Writes() - before; got != 1 {
		t.Fatalf("after a pool Exec writes = %d, want 1", got)
	}
	err := d.WithBoundTx(func(b *db.DB) error {
		if _, err := b.Exec("INSERT INTO t VALUES(2)"); err != nil {
			return err
		}
		if got := d.Writes() - before; got != 1 {
			t.Errorf("inside an open transaction writes = %d, want 1", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Writes() - before; got != 2 {
		t.Fatalf("after a transaction writes = %d, want 2", got)
	}
	_ = d.WithTx(func(*db.Tx) error { return errors.New("roll back") })
	if got := d.Writes() - before; got != 3 {
		t.Fatalf("after a rolled-back transaction writes = %d, want 3", got)
	}
	if err := d.WithReadTx(func(tx *db.Tx) error { countRows(t, tx); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := d.Writes() - before; got != 3 {
		t.Fatalf("a read transaction counted: writes = %d, want 3", got)
	}
}

func TestWithBoundTxMisusePanics(t *testing.T) {
	d := openBoundTestDB(t)
	var kept *db.DB
	_ = d.WithBoundTx(func(b *db.DB) error {
		kept = b
		for name, misuse := range map[string]func(){
			"Close":      func() { _ = b.Close() },
			"SqlDB":      func() { _ = b.SqlDB() },
			"Checkpoint": func() { _ = b.Checkpoint(db.CheckpointPassive) },
		} {
			if !panics(misuse) {
				t.Errorf("%s on a bound DB did not panic", name)
			}
		}
		return nil
	})
	if !panics(func() { _, _ = kept.Exec("INSERT INTO t VALUES(1)") }) {
		t.Fatal("a bound DB used after its transaction ended did not panic")
	}
}

func panics(fn func()) (did bool) {
	defer func() { did = recover() != nil }()
	fn()
	return false
}
