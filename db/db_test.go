package db_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/libfossil/db"
	_ "github.com/danmestas/libfossil/internal/testdriver"
	"github.com/danmestas/libfossil/simio"
)

func TestOpenClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	var result int
	err = d.QueryRow("SELECT 1+1").Scan(&result)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if result != 2 {
		t.Fatalf("got %d, want 2", result)
	}
}
func TestOpenSQLMergesURIParams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	conn, err := db.OpenSQL(path, db.OpenConfig{}, nil)
	if err != nil {
		t.Fatalf("OpenSQL create: %v", err)
	}
	if _, err := conn.Exec("CREATE TABLE t(v INTEGER)"); err != nil {
		conn.Close()
		t.Fatalf("create table: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close create conn: %v", err)
	}

	driver, dsn := db.SQLDriverAndDSN(path, db.OpenConfig{}, map[string]string{"mode": "ro"})
	if driver == "" {
		t.Fatal("empty driver")
	}
	if strings.Count(dsn, "?") != 1 {
		t.Fatalf("DSN %q contains %d query delimiters, want 1", dsn, strings.Count(dsn, "?"))
	}
	if !strings.Contains(dsn, "mode=ro") {
		t.Fatalf("DSN %q missing mode=ro", dsn)
	}

	ro, err := db.OpenSQL(path, db.OpenConfig{}, map[string]string{"mode": "ro"})
	if err != nil {
		t.Fatalf("OpenSQL read-only: %v", err)
	}
	defer ro.Close()
	if _, err := ro.Exec("INSERT INTO t(v) VALUES(1)"); err == nil {
		t.Fatal("read-only OpenSQL accepted a write")
	}
}

func TestExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	_, err = d.Exec("CREATE TABLE test(id INTEGER PRIMARY KEY, val TEXT)")
	if err != nil {
		t.Fatalf("Exec CREATE: %v", err)
	}

	_, err = d.Exec("INSERT INTO test(val) VALUES(?)", "hello")
	if err != nil {
		t.Fatalf("Exec INSERT: %v", err)
	}

	var val string
	err = d.QueryRow("SELECT val FROM test WHERE id=1").Scan(&val)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if val != "hello" {
		t.Fatalf("val = %q, want %q", val, "hello")
	}
}

func TestApplicationID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	err = d.SetApplicationID(252006673)
	if err != nil {
		t.Fatalf("SetApplicationID: %v", err)
	}

	id, err := d.ApplicationID()
	if err != nil {
		t.Fatalf("ApplicationID: %v", err)
	}
	if id != 252006673 {
		t.Fatalf("application_id = %d, want 252006673", id)
	}
}

func TestTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if _, err := d.Exec("CREATE TABLE test(id INTEGER PRIMARY KEY, val TEXT)"); err != nil {
		t.Fatalf("Exec CREATE: %v", err)
	}

	// Commit case
	err = d.WithTx(func(tx *db.Tx) error {
		_, err := tx.Exec("INSERT INTO test(val) VALUES(?)", "committed")
		return err
	})
	if err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}

	var count int
	if err := d.QueryRow("SELECT count(*) FROM test").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("count after commit = %d, want 1", count)
	}

	// Rollback case
	err = d.WithTx(func(tx *db.Tx) error {
		if _, err := tx.Exec("INSERT INTO test(val) VALUES(?)", "rolled-back"); err != nil {
			return err
		}
		return fmt.Errorf("deliberate error")
	})
	if err == nil {
		t.Fatal("WithTx should return error")
	}

	if err := d.QueryRow("SELECT count(*) FROM test").Scan(&count); err != nil {
		t.Fatalf("count query after rollback: %v", err)
	}
	if count != 1 {
		t.Fatalf("count after rollback = %d, want 1", count)
	}
}

func TestCreateRepoSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	err = db.CreateRepoSchema(d)
	if err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}

	// Verify repo1 (static) tables exist
	repo1Tables := []string{"blob", "delta", "rcvfrom", "user", "config", "shun", "private", "reportfmt", "concealed"}
	for _, table := range repo1Tables {
		var name string
		err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("repo1 table %q not found: %v", table, err)
		}
	}

	// Verify repo2 (transient) tables exist
	repo2Tables := []string{"filename", "mlink", "plink", "leaf", "event", "phantom", "orphan", "unclustered", "unsent", "tag", "tagxref", "backlink", "attachment", "cherrypick", "forumpost"}
	for _, table := range repo2Tables {
		var name string
		err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("repo2 table %q not found: %v", table, err)
		}
	}

	// Verify application_id
	id, err := d.ApplicationID()
	if err != nil {
		t.Fatalf("ApplicationID: %v", err)
	}
	if id != 252006673 {
		t.Fatalf("application_id = %d, want 252006673", id)
	}

	// Verify seed rcvfrom row
	var rcvid int
	err = d.QueryRow("SELECT rcvid FROM rcvfrom WHERE rcvid=1").Scan(&rcvid)
	if err != nil {
		t.Fatalf("seed rcvfrom row missing: %v", err)
	}

	// Verify seed tag rows (1-11)
	var tagCount int
	err = d.QueryRow("SELECT count(*) FROM tag").Scan(&tagCount)
	if err != nil {
		t.Fatalf("tag count: %v", err)
	}
	if tagCount != 11 {
		t.Fatalf("tag count = %d, want 11", tagCount)
	}

	// Verify specific seed tags
	var tagName string
	err = d.QueryRow("SELECT tagname FROM tag WHERE tagid=8").Scan(&tagName)
	if err != nil {
		t.Fatalf("tag 8: %v", err)
	}
	if tagName != "branch" {
		t.Fatalf("tag 8 name = %q, want %q", tagName, "branch")
	}
}

func TestCreateRepoSchema_FossilValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fossil CLI validation in short mode")
	}

	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	err = db.CreateRepoSchema(d)
	if err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}

	err = db.SeedConfig(d, simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("SeedConfig: %v", err)
	}

	err = db.SeedUser(d, "testuser")
	if err != nil {
		t.Fatalf("SeedUser: %v", err)
	}

	d.Close()

	// fossil rebuild should pass on Go-created schema
	cmd := exec.Command("fossil", "rebuild", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}
}

func TestRegisterDriver(t *testing.T) {
	// Driver should already be registered by testdriver import (added in Task 3).
	cfg := db.RegisteredDriver()
	if cfg == nil {
		t.Fatal("no driver registered")
	}
	if cfg.Name == "" {
		t.Fatal("registered driver name is empty")
	}
	t.Logf("active driver: %s", cfg.Name)

	dsn := cfg.BuildDSN("/tmp/test.db", db.DefaultPragmas())
	t.Logf("DSN: %s", dsn)
	if !strings.Contains(dsn, "journal_mode") {
		t.Fatal("DSN missing journal_mode pragma")
	}
	if !strings.Contains(dsn, "busy_timeout") {
		t.Fatal("DSN missing busy_timeout pragma")
	}
}

func TestOpenWithDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if d.Driver() == "" {
		t.Fatal("Driver() returned empty string")
	}
	t.Logf("opened with driver: %s", d.Driver())

	var mode string
	if err := d.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode query: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestOpenWithCustomPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.OpenWith(path, db.OpenConfig{
		Pragmas: map[string]string{
			"cache_size": "-2000",
		},
	})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer d.Close()

	var mode string
	if err := d.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode query: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal (default should still apply)", mode)
	}
}
