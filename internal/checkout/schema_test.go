package checkout

import (
	"database/sql"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// openTestCkoutDB opens an in-memory checkout DB for testing.
func openTestCkoutDB(t *testing.T) *sql.DB {
	t.Helper()
	drv := db.RegisteredDriver()
	if drv == nil {
		t.Fatal("no SQLite driver registered")
	}
	ckdb, err := sql.Open(drv.Name, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ckdb.Close() })
	return ckdb
}

func TestEnsureTables(t *testing.T) {
	ckdb := openTestCkoutDB(t)

	// First call: create tables.
	if err := EnsureTables(ckdb); err != nil {
		t.Fatalf("EnsureTables failed: %v", err)
	}

	// Verify tables exist.
	tables := []string{"vfile", "vmerge", "vvar"}
	for _, table := range tables {
		var name string
		err := ckdb.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}

	// Second call: idempotent.
	if err := EnsureTables(ckdb); err != nil {
		t.Errorf("EnsureTables (second call) failed: %v", err)
	}
}

func TestVVarRoundTrip(t *testing.T) {
	ckdb := openTestCkoutDB(t)
	if err := EnsureTables(ckdb); err != nil {
		t.Fatalf("EnsureTables failed: %v", err)
	}

	// Set and get.
	const name, value = "test-key", "test-value"
	if err := setVVar(ckdb, name, value); err != nil {
		t.Fatalf("setVVar failed: %v", err)
	}

	got, err := getVVar(ckdb, name)
	if err != nil {
		t.Fatalf("getVVar failed: %v", err)
	}
	if got != value {
		t.Errorf("getVVar(%q) = %q, want %q", name, got, value)
	}

	// Get non-existent key.
	got, err = getVVar(ckdb, "nonexistent")
	if err != nil {
		t.Errorf("getVVar(nonexistent) failed: %v", err)
	}
	if got != "" {
		t.Errorf("getVVar(nonexistent) = %q, want empty string", got)
	}
}
