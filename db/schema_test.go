package db_test

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
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

// A new repo is seeded with a hash-policy so it names artifacts the way
// `fossil init` does. Without the row, naming falls back to deriving from
// the repo's own artifacts, and an empty repo has none — so it would settle
// on SHA1 and stay there permanently.
func TestSeedConfigWritesHashPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := db.CreateRepoSchema(d); err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}
	if err := db.SeedConfig(d, simio.CryptoRand{}, ""); err != nil {
		t.Fatalf("SeedConfig: %v", err)
	}

	var got string
	if err := d.QueryRow("SELECT value FROM config WHERE name='hash-policy'").Scan(&got); err != nil {
		t.Fatalf("no hash-policy row: %v", err)
	}
	if want := strconv.Itoa(db.HashPolicySHA3); got != want {
		t.Errorf("hash-policy = %q, want %q (sha3, what fossil init writes)", got, want)
	}
}
