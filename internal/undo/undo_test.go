//go:build !js

package undo

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	libdb "github.com/danmestas/go-libfossil/db"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// setupCheckoutDB creates a minimal .fslckout DB with vvar, vfile, vmerge tables
// and two test files (a.txt, b.txt) both on disk and in vfile.
func setupCheckoutDB(t *testing.T, dir string) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(dir, ".fslckout")
	db, err := sql.Open(libdb.RegisteredDriver().Name, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE vvar(name TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE vfile(
			id       INTEGER PRIMARY KEY,
			vid      INTEGER,
			chnged   INTEGER DEFAULT 0,
			deleted  INTEGER DEFAULT 0,
			isexe    BOOLEAN DEFAULT 0,
			islink   BOOLEAN DEFAULT 0,
			rid      INTEGER DEFAULT 0,
			mrid     INTEGER DEFAULT 0,
			mtime    REAL DEFAULT 0,
			pathname TEXT,
			origname TEXT
		)`,
		`CREATE TABLE vmerge(id INTEGER, merge INTEGER, mhash TEXT)`,
		`INSERT INTO vvar(name,value) VALUES('checkout','10')`,
		`INSERT INTO vfile(id,vid,pathname,rid) VALUES(1, 10, 'a.txt', 100)`,
		`INSERT INTO vfile(id,vid,pathname,rid) VALUES(2, 10, 'b.txt', 200)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v\n  SQL: %s", err, s)
		}
	}

	// Write real files on disk.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bravo"), 0o644); err != nil {
		t.Fatal(err)
	}

	return db
}

func readVVar(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var val string
	if err := db.QueryRow("SELECT value FROM vvar WHERE name=?", name).Scan(&val); err != nil {
		t.Fatalf("readVVar(%s): %v", name, err)
	}
	return val
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readFile(%s): %v", path, err)
	}
	return string(data)
}


func readVFileRID(t *testing.T, db *sql.DB, pathname string) int {
	t.Helper()
	var rid int
	if err := db.QueryRow("SELECT rid FROM vfile WHERE pathname=?", pathname).Scan(&rid); err != nil {
		t.Fatalf("readVFileRID(%s): %v", pathname, err)
	}
	return rid
}

func TestSaveAndUndo(t *testing.T) {
	dir := t.TempDir()
	db := setupCheckoutDB(t, dir)

	// Save current state.
	if err := Save(db, dir, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Modify file and vfile.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("MODIFIED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE vfile SET rid=999 WHERE pathname='a.txt'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("REPLACE INTO vvar(name,value) VALUES('checkout','20')"); err != nil {
		t.Fatal(err)
	}

	// Undo.
	if err := Undo(db, dir); err != nil {
		t.Fatalf("Undo: %v", err)
	}

	// Verify file restored.
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "alpha" {
		t.Errorf("a.txt after undo: got %q, want %q", got, "alpha")
	}

	// Verify vfile restored.
	if rid := readVFileRID(t, db, "a.txt"); rid != 100 {
		t.Errorf("vfile rid after undo: got %d, want 100", rid)
	}

	// Verify checkout restored.
	if v := readVVar(t, db, "checkout"); v != "10" {
		t.Errorf("checkout after undo: got %q, want %q", v, "10")
	}

	// Verify undo_available == 2.
	if v := readVVar(t, db, "undo_available"); v != "2" {
		t.Errorf("undo_available after undo: got %q, want %q", v, "2")
	}
}

func TestRedo(t *testing.T) {
	dir := t.TempDir()
	db := setupCheckoutDB(t, dir)

	// Save, modify, undo.
	if err := Save(db, dir, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("MODIFIED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE vfile SET rid=999 WHERE pathname='a.txt'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("REPLACE INTO vvar(name,value) VALUES('checkout','20')"); err != nil {
		t.Fatal(err)
	}
	if err := Undo(db, dir); err != nil {
		t.Fatal(err)
	}

	// Redo.
	if err := Redo(db, dir); err != nil {
		t.Fatalf("Redo: %v", err)
	}

	// Verify modification restored.
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "MODIFIED" {
		t.Errorf("a.txt after redo: got %q, want %q", got, "MODIFIED")
	}

	// Verify vfile has modified rid.
	if rid := readVFileRID(t, db, "a.txt"); rid != 999 {
		t.Errorf("vfile rid after redo: got %d, want 999", rid)
	}

	// Verify checkout restored to modified value.
	if v := readVVar(t, db, "checkout"); v != "20" {
		t.Errorf("checkout after redo: got %q, want %q", v, "20")
	}

	// Verify undo_available == 1.
	if v := readVVar(t, db, "undo_available"); v != "1" {
		t.Errorf("undo_available after redo: got %q, want %q", v, "1")
	}
}

func TestUndoNotAvailable(t *testing.T) {
	dir := t.TempDir()
	db := setupCheckoutDB(t, dir)

	// Undo without prior save should fail.
	if err := Undo(db, dir); err == nil {
		t.Fatal("expected error from Undo without Save, got nil")
	}

	// Redo without prior undo should also fail.
	if err := Redo(db, dir); err == nil {
		t.Fatal("expected error from Redo without Undo, got nil")
	}
}

func TestSaveReplacesOldUndo(t *testing.T) {
	dir := t.TempDir()
	db := setupCheckoutDB(t, dir)

	// First save (captures "alpha"/"bravo").
	if err := Save(db, dir, nil); err != nil {
		t.Fatal(err)
	}

	// Modify files.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE vfile SET rid=500 WHERE pathname='a.txt'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("REPLACE INTO vvar(name,value) VALUES('checkout','30')"); err != nil {
		t.Fatal(err)
	}

	// Second save (captures "second" state).
	if err := Save(db, dir, nil); err != nil {
		t.Fatal(err)
	}

	// Modify again.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("third"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE vfile SET rid=600 WHERE pathname='a.txt'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("REPLACE INTO vvar(name,value) VALUES('checkout','40')"); err != nil {
		t.Fatal(err)
	}

	// Undo should restore the second save (not the first).
	if err := Undo(db, dir); err != nil {
		t.Fatalf("Undo: %v", err)
	}

	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "second" {
		t.Errorf("a.txt after undo: got %q, want %q", got, "second")
	}
	if rid := readVFileRID(t, db, "a.txt"); rid != 500 {
		t.Errorf("vfile rid after undo: got %d, want 500", rid)
	}
	if v := readVVar(t, db, "checkout"); v != "30" {
		t.Errorf("checkout after undo: got %q, want %q", v, "30")
	}
}
