package db_test

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/db"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func TestSeedNobody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := db.CreateRepoSchema(d); err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}
	if err := db.SeedNobody(d, "oi"); err != nil {
		t.Fatalf("SeedNobody: %v", err)
	}

	var login, cap string
	err = d.QueryRow("SELECT login, cap FROM user WHERE login='nobody'").Scan(&login, &cap)
	if err != nil {
		t.Fatalf("nobody user not found: %v", err)
	}
	if cap != "oi" {
		t.Errorf("cap = %q, want oi", cap)
	}
}

// TestCreateRepoSchemaTicketTables asserts that a newly created repository
// provisions the ticket, ticketchng tables and the ticketchng_idx1 index,
// matching what canonical `fossil new` produces. Stock Fossil's web UI
// queries the ticket table unconditionally (e.g. the artifact-prefix lookup
// behind /info/<uuid>), so a repo missing this schema crashes stock tooling.
func TestCreateRepoSchemaTicketTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := db.CreateRepoSchema(d); err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}

	for _, tbl := range []string{"ticket", "ticketchng"} {
		var name string
		err := d.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", tbl, err)
		}
	}

	var idxName string
	err = d.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='ticketchng_idx1'",
	).Scan(&idxName)
	if err != nil {
		t.Errorf("index ticketchng_idx1 not found: %v", err)
	}
}
