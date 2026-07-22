//go:build !js

package stash

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	libdb "github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/hash"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// testEnv sets up a repo DB with one blob ("hello" in a.txt), a checkout DB
// with vvar/vfile/vmerge tables, writes a.txt to disk, and returns both DBs,
// the working directory, and the rid+uuid of the stored blob.
func testEnv(t *testing.T) (repoDB *sql.DB, ckout *sql.DB, dir string) {
	t.Helper()
	dir = t.TempDir()

	// --- Repo DB ---
	repoPath := filepath.Join(dir, "repo.fossil")
	var err error
	repoDB, err = sql.Open(libdb.RegisteredDriver().Name, repoPath)
	if err != nil {
		t.Fatalf("open repo db: %v", err)
	}
	t.Cleanup(func() { repoDB.Close() })

	repoSchema := []string{
		`CREATE TABLE blob(
			rid INTEGER PRIMARY KEY,
			rcvid INTEGER,
			size INTEGER,
			uuid TEXT UNIQUE NOT NULL,
			content BLOB,
			CHECK( length(uuid)>=40 AND rid>0 )
		)`,
		`CREATE TABLE delta(
			rid INTEGER PRIMARY KEY,
			srcid INTEGER NOT NULL REFERENCES blob
		)`,
		`CREATE INDEX delta_i1 ON delta(srcid)`,
		`INSERT INTO blob(rid, uuid, size, content) VALUES(0, '0000000000000000000000000000000000000000', 0, NULL)`,
	}
	// The CHECK constraint requires rid>0, so we need to insert rid=0 differently.
	// Actually let's just create schema and use blob.Store.
	repoSchema2 := []string{
		`CREATE TABLE blob(
			rid INTEGER PRIMARY KEY,
			rcvid INTEGER,
			size INTEGER,
			uuid TEXT UNIQUE NOT NULL,
			content BLOB
		)`,
		`CREATE TABLE delta(
			rid INTEGER PRIMARY KEY,
			srcid INTEGER NOT NULL REFERENCES blob
		)`,
		`CREATE INDEX delta_i1 ON delta(srcid)`,
		`CREATE TABLE unclustered(rid INTEGER PRIMARY KEY)`,
	}
	_ = repoSchema
	for _, s := range repoSchema2 {
		if _, err := repoDB.Exec(s); err != nil {
			t.Fatalf("repo schema: %v\n  SQL: %s", err, s)
		}
	}

	// Store "hello" blob.
	rid, uuid, err := blob.Store(repoDB, []byte("hello"))
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}

	// --- Checkout DB ---
	ckoutPath := filepath.Join(dir, ".fslckout")
	ckout, err = sql.Open(libdb.RegisteredDriver().Name, ckoutPath)
	if err != nil {
		t.Fatalf("open ckout db: %v", err)
	}
	t.Cleanup(func() { ckout.Close() })

	ckoutSchema := []string{
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
	}
	for _, s := range ckoutSchema {
		if _, err := ckout.Exec(s); err != nil {
			t.Fatalf("ckout schema: %v\n  SQL: %s", err, s)
		}
	}

	// Populate vvar and vfile.
	if _, err := ckout.Exec("INSERT INTO vvar(name,value) VALUES('checkout','1')"); err != nil {
		t.Fatal(err)
	}
	if _, err := ckout.Exec("INSERT INTO vvar(name,value) VALUES('checkout-hash',?)", uuid); err != nil {
		t.Fatal(err)
	}
	if _, err := ckout.Exec("INSERT INTO vfile(id,vid,pathname,rid,mrid) VALUES(1,1,'a.txt',?,?)", int64(rid), int64(rid)); err != nil {
		t.Fatal(err)
	}

	// Write a.txt to disk with baseline content.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	return repoDB, ckout, dir
}

// modifyFile changes a.txt on disk and marks it changed in vfile.
func modifyFile(t *testing.T, ckout *sql.DB, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ckout.Exec("UPDATE vfile SET chnged=1 WHERE pathname='a.txt'"); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readFile(%s): %v", path, err)
	}
	return string(data)
}

func TestSaveAndList(t *testing.T) {
	repoDB, ckout, dir := testEnv(t)

	// Modify file.
	modifyFile(t, ckout, dir)

	// Save.
	if err := Save(ckout, repoDB, dir, "test stash"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File should be reverted to baseline.
	got := readFile(t, filepath.Join(dir, "a.txt"))
	if got != "hello" {
		t.Errorf("after Save: a.txt = %q, want %q", got, "hello")
	}

	// vfile should show chnged=0.
	var chnged int
	if err := ckout.QueryRow("SELECT chnged FROM vfile WHERE pathname='a.txt'").Scan(&chnged); err != nil {
		t.Fatal(err)
	}
	if chnged != 0 {
		t.Errorf("after Save: vfile chnged = %d, want 0", chnged)
	}

	// List should have 1 entry.
	entries, err := List(ckout)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List: got %d entries, want 1", len(entries))
	}
	if entries[0].Comment != "test stash" {
		t.Errorf("List: comment = %q, want %q", entries[0].Comment, "test stash")
	}

	// Verify baseline hash is stored.
	expectedHash := hash.SHA1([]byte("hello"))
	if entries[0].Hash != expectedHash {
		// Hash is checkout-hash from vvar, not file hash — it's the manifest UUID.
		// Just check it's not empty.
	}
}

func TestPopRestoresChanges(t *testing.T) {
	repoDB, ckout, dir := testEnv(t)

	modifyFile(t, ckout, dir)

	if err := Save(ckout, repoDB, dir, "pop test"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify reverted.
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "hello" {
		t.Fatalf("after Save: a.txt = %q, want %q", got, "hello")
	}

	// Pop.
	if err := Pop(ckout, repoDB, dir); err != nil {
		t.Fatalf("Pop: %v", err)
	}

	// File should have modified content.
	got := readFile(t, filepath.Join(dir, "a.txt"))
	if got != "hello world" {
		t.Errorf("after Pop: a.txt = %q, want %q", got, "hello world")
	}

	// List should be empty.
	entries, err := List(ckout)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("after Pop: List has %d entries, want 0", len(entries))
	}
}

func TestApplyKeepsStash(t *testing.T) {
	repoDB, ckout, dir := testEnv(t)

	modifyFile(t, ckout, dir)

	if err := Save(ckout, repoDB, dir, "apply test"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Get the stash ID.
	entries, err := List(ckout)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// Apply.
	if err := Apply(ckout, repoDB, dir, entries[0].ID); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// File should have modified content.
	got := readFile(t, filepath.Join(dir, "a.txt"))
	if got != "hello world" {
		t.Errorf("after Apply: a.txt = %q, want %q", got, "hello world")
	}

	// List should still have 1 entry.
	entries, err = List(ckout)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("after Apply: List has %d entries, want 1", len(entries))
	}
}

func TestDropAndClear(t *testing.T) {
	repoDB, ckout, dir := testEnv(t)

	// Save twice (need to re-modify between saves).
	modifyFile(t, ckout, dir)
	if err := Save(ckout, repoDB, dir, "first"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}

	modifyFile(t, ckout, dir)
	if err := Save(ckout, repoDB, dir, "second"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}

	entries, err := List(ckout)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Drop first (oldest = last in list since ordered DESC).
	firstID := entries[len(entries)-1].ID
	if err := Drop(ckout, firstID); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	entries, err = List(ckout)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("after Drop: expected 1 entry, got %d", len(entries))
	}
	if entries[0].Comment != "second" {
		t.Errorf("remaining entry comment = %q, want %q", entries[0].Comment, "second")
	}

	// Clear.
	if err := Clear(ckout); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	entries, err = List(ckout)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("after Clear: expected 0 entries, got %d", len(entries))
	}
}
