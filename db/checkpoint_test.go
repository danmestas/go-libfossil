package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestCloseCheckpointsWAL verifies that Close runs a TRUNCATE checkpoint so
// the on-disk file is readable by external SQLite/fossil tooling — which
// reads the main database file and rejects WAL-stub repos.
func TestCloseCheckpointsWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}

	if _, err := d.Exec("CREATE TABLE t (k INTEGER PRIMARY KEY AUTOINCREMENT, v BLOB)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	payload := make([]byte, 2048)
	for i := range 64 {
		if _, err := d.Exec("INSERT INTO t(v) VALUES(?)", payload); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	walBefore, walStatErr := os.Stat(path + "-wal")
	if walStatErr != nil || walBefore.Size() == 0 {
		t.Skipf("no populated WAL file before close (driver=%s) — likely non-WAL journal mode", d.Driver())
	}

	if err := d.Close(); err != nil {
		t.Fatalf("d.Close: %v", err)
	}

	mainAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat main after close: %v", err)
	}
	if mainAfter.Size() <= 4096 {
		t.Errorf("main file is still a stub after close: got %d bytes, want > 4096", mainAfter.Size())
	}
	if walAfter, statErr := os.Stat(path + "-wal"); statErr == nil && walAfter.Size() > 0 {
		t.Errorf("expected WAL file empty or absent after close, got %d bytes", walAfter.Size())
	}

	d2, err := db.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = d2.Close() })
	var n int
	if err := d2.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count after reopen: %v", err)
	}
	if n != 64 {
		t.Errorf("row count after reopen: got %d, want 64", n)
	}
}

// TestCheckpointMidFlight verifies that Checkpoint can be called on a live
// database for both PASSIVE (non-blocking) and TRUNCATE (compacting) modes.
func TestCheckpointMidFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if _, err := d.Exec("CREATE TABLE t (k INTEGER PRIMARY KEY AUTOINCREMENT)"); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	for range 32 {
		if _, err := d.Exec("INSERT INTO t DEFAULT VALUES"); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	if err := d.Checkpoint(db.CheckpointPassive); err != nil {
		t.Errorf("Checkpoint(PASSIVE): %v", err)
	}
	if err := d.Checkpoint(db.CheckpointTruncate); err != nil {
		t.Errorf("Checkpoint(TRUNCATE): %v", err)
	}
}

func TestCheckpointModeString(t *testing.T) {
	cases := []struct {
		mode db.CheckpointMode
		want string
	}{
		{db.CheckpointPassive, "PASSIVE"},
		{db.CheckpointFull, "FULL"},
		{db.CheckpointRestart, "RESTART"},
		{db.CheckpointTruncate, "TRUNCATE"},
	}
	for _, tc := range cases {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", int(tc.mode), got, tc.want)
		}
	}
}

func TestCheckpointInvalidMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := d.Checkpoint(db.CheckpointMode(-1)); err == nil {
		t.Errorf("Checkpoint(-1): want error, got nil")
	}
	if err := d.Checkpoint(db.CheckpointMode(99)); err == nil {
		t.Errorf("Checkpoint(99): want error, got nil")
	}
}
