package repo

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/simio"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func TestCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()

	if r.Path() != path {
		t.Fatalf("Path = %q, want %q", r.Path(), path)
	}
}

func TestCreate_FossilValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fossil validation")
	}
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.Close()

	// fossil rebuild must pass (no --verify flag in Fossil 2.28)
	cmd := exec.Command("fossil", "rebuild", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}
}

func TestOpenFossilCreated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fossil validation")
	}
	path := filepath.Join(t.TempDir(), "fossil-created.fossil")
	cmd := exec.Command("fossil", "new", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil new: %v\n%s", err, out)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	if r.Path() != path {
		t.Fatalf("Path = %q, want %q", r.Path(), path)
	}
}

func TestOpen_NotARepo(t *testing.T) {
	// Create a plain SQLite database with no application_id
	path := filepath.Join(t.TempDir(), "not-a-repo.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	d.Exec("CREATE TABLE test(x)")
	d.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("expected error opening non-repo database")
	}
}

func TestOpen_NonExistent(t *testing.T) {
	_, err := Open("/tmp/nonexistent-repo-12345.fossil")
	if err == nil {
		t.Fatal("expected error opening nonexistent file")
	}
}

func TestWithTx(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()

	err = r.WithTx(func(tx *db.Tx) error {
		_, err := tx.Exec("INSERT INTO config(name,value,mtime) VALUES('test-key','test-val',0)")
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	var val string
	r.DB().QueryRow("SELECT value FROM config WHERE name='test-key'").Scan(&val)
	if val != "test-val" {
		t.Fatalf("val = %q, want %q", val, "test-val")
	}
}

func TestRoundTrip_FossilValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fossil validation")
	}

	// 1. fossil creates repo
	fossilPath := filepath.Join(t.TempDir(), "fossil-created.fossil")
	exec.Command("fossil", "new", fossilPath).CombinedOutput()

	// 2. Go opens it
	r1, err := Open(fossilPath)
	if err != nil {
		t.Fatalf("Open fossil-created: %v", err)
	}
	r1.Close()

	// 3. Go creates repo
	goPath := filepath.Join(t.TempDir(), "go-created.fossil")
	r2, err := Create(goPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r2.Close()

	// 4. fossil validates Go-created repo
	cmd := exec.Command("fossil", "rebuild", goPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild go-created failed: %v\n%s", err, out)
	}

	// 5. fossil queries Go-created repo
	cmd = exec.Command("fossil", "sql", "-R", goPath, "SELECT count(*) FROM blob;")
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("fossil sql go-created: %v", err)
	}
}
