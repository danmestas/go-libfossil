package libfossil_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/libfossil"
	"github.com/danmestas/libfossil/internal/annotate"
	"github.com/danmestas/libfossil/internal/bisect"
	libdb "github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/path"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/stash"
	"github.com/danmestas/libfossil/internal/undo"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// setupCheckoutDB creates a .fslckout database with vvar, vfile, and vmerge tables.
func setupCheckoutDB(t *testing.T, dir string, rid libfossil.FslID, hash string) *sql.DB {
	t.Helper()
	ckoutPath := filepath.Join(dir, ".fslckout")
	ckout, err := sql.Open(libdb.RegisteredDriver().Name, ckoutPath)
	if err != nil {
		t.Fatalf("open checkout db: %v", err)
	}
	t.Cleanup(func() { ckout.Close() })

	stmts := []string{
		`CREATE TABLE vvar(name TEXT PRIMARY KEY, value CLOB) WITHOUT ROWID`,
		`CREATE TABLE vfile(id INTEGER PRIMARY KEY, vid INTEGER, chnged INT DEFAULT 0,
			deleted BOOLEAN DEFAULT 0, isexe BOOLEAN, islink BOOLEAN, rid INTEGER,
			mrid INTEGER, mtime INTEGER, pathname TEXT, origname TEXT, mhash TEXT,
			UNIQUE(pathname, vid))`,
		`CREATE TABLE vmerge(id INTEGER, merge INTEGER, mhash TEXT)`,
	}
	for _, s := range stmts {
		if _, err := ckout.Exec(s); err != nil {
			t.Fatalf("create checkout table: %v", err)
		}
	}

	vvars := [][2]string{
		{"checkout", intStr(rid)},
		{"checkout-hash", hash},
		{"undo_available", "0"},
		{"undo_checkout", "0"},
		{"stash-next", "1"},
	}
	for _, v := range vvars {
		if _, err := ckout.Exec("INSERT INTO vvar VALUES(?,?)", v[0], v[1]); err != nil {
			t.Fatalf("insert vvar %s: %v", v[0], err)
		}
	}
	return ckout
}

func intStr(id libfossil.FslID) string {
	return fmt.Sprintf("%d", id)
}

// TestIntegrationStash verifies the stash save/pop round-trip with a real repo.
func TestIntegrationStash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	repoPath := filepath.Join(dir, "stash-test.fossil")

	// Create repo and checkin a file.
	r, err := repo.Create(repoPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create repo: %v", err)
	}
	defer r.Close()

	rid, hash, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello world\n")}},
		Comment: "initial",
		User:    "testuser",
		Time:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Look up the blob RID for hello.txt content.
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	var fileRID libfossil.FslID
	r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", files[0].UUID).Scan(&fileRID)

	// Set up checkout DB.
	ckout := setupCheckoutDB(t, dir, rid, hash)

	// Write the original file to disk and register in vfile.
	filePath := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(filePath, []byte("hello world\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := ckout.Exec("INSERT INTO vfile(vid, pathname, rid, mrid, chnged, isexe, islink) VALUES(?,?,?,?,0,0,0)",
		rid, "hello.txt", fileRID, fileRID); err != nil {
		t.Fatalf("insert vfile: %v", err)
	}

	// Modify the file and mark as changed.
	if err := os.WriteFile(filePath, []byte("hello modified\n"), 0o644); err != nil {
		t.Fatalf("modify file: %v", err)
	}
	if _, err := ckout.Exec("UPDATE vfile SET chnged=1 WHERE pathname='hello.txt'"); err != nil {
		t.Fatalf("update vfile chnged: %v", err)
	}

	// Stash save — should revert file to original.
	if err := stash.Save(ckout, r.DB().SqlDB(), dir, "test stash"); err != nil {
		t.Fatalf("stash.Save: %v", err)
	}

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read reverted file: %v", err)
	}
	if string(got) != "hello world\n" {
		t.Fatalf("after stash save: got %q, want %q", got, "hello world\n")
	}

	// Stash pop — should restore modified content.
	if err := stash.Pop(ckout, r.DB().SqlDB(), dir); err != nil {
		t.Fatalf("stash.Pop: %v", err)
	}

	got, err = os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read popped file: %v", err)
	}
	if string(got) != "hello modified\n" {
		t.Fatalf("after stash pop: got %q, want %q", got, "hello modified\n")
	}

	// Verify stash list is empty.
	entries, err := stash.List(ckout)
	if err != nil {
		t.Fatalf("stash.List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("stash list should be empty, got %d entries", len(entries))
	}
}

// TestIntegrationUndo verifies the undo/redo cycle on checkout files.
func TestIntegrationUndo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()

	// Set up checkout DB with a dummy rid.
	ckout := setupCheckoutDB(t, dir, 1, "abc123")

	filePath := filepath.Join(dir, "test.txt")
	originalContent := []byte("original content\n")
	if err := os.WriteFile(filePath, originalContent, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Register in vfile.
	if _, err := ckout.Exec("INSERT INTO vfile(vid, pathname, rid, mrid, chnged, isexe, islink) VALUES(1,'test.txt',1,1,0,0,0)"); err != nil {
		t.Fatalf("insert vfile: %v", err)
	}

	// Save undo state.
	if err := undo.Save(ckout, dir, nil); err != nil {
		t.Fatalf("undo.Save: %v", err)
	}

	// Modify file on disk.
	modifiedContent := []byte("modified content\n")
	if err := os.WriteFile(filePath, modifiedContent, 0o644); err != nil {
		t.Fatalf("modify file: %v", err)
	}

	// Undo — should restore original.
	if err := undo.Undo(ckout, dir); err != nil {
		t.Fatalf("undo.Undo: %v", err)
	}

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read after undo: %v", err)
	}
	if string(got) != string(originalContent) {
		t.Fatalf("after undo: got %q, want %q", got, originalContent)
	}

	// Redo — should restore modification.
	if err := undo.Redo(ckout, dir); err != nil {
		t.Fatalf("undo.Redo: %v", err)
	}

	got, err = os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read after redo: %v", err)
	}
	if string(got) != string(modifiedContent) {
		t.Fatalf("after redo: got %q, want %q", got, modifiedContent)
	}
}

// TestIntegrationAnnotate verifies line-by-line blame attribution across 2 commits.
func TestIntegrationAnnotate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	repoPath := filepath.Join(dir, "annotate-test.fossil")

	r, err := repo.Create(repoPath, "alice", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create repo: %v", err)
	}
	defer r.Close()

	// Commit 1 by alice: "A\nB\n"
	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte("A\nB\n")}},
		Comment: "commit 1",
		User:    "alice",
		Time:    time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin 1: %v", err)
	}

	// Commit 2 by bob: "A\nC\n" (B changed to C)
	rid2, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte("A\nC\n")}},
		Comment: "commit 2",
		User:    "bob",
		Parent:  rid1,
		Time:    time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin 2: %v", err)
	}

	lines, err := annotate.Annotate(r, annotate.Options{
		FilePath: "file.txt",
		StartRID: rid2,
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}

	// Line 1 ("A") should be attributed to alice (unchanged from commit 1).
	if lines[0].Text != "A" {
		t.Fatalf("line 0 text = %q, want %q", lines[0].Text, "A")
	}
	if lines[0].Version.User != "alice" {
		t.Fatalf("line 0 user = %q, want %q", lines[0].Version.User, "alice")
	}

	// Line 2 ("C") should be attributed to bob (changed in commit 2).
	if lines[1].Text != "C" {
		t.Fatalf("line 1 text = %q, want %q", lines[1].Text, "C")
	}
	if lines[1].Version.User != "bob" {
		t.Fatalf("line 1 user = %q, want %q", lines[1].Version.User, "bob")
	}

	_ = rid2 // used above
}

// TestIntegrationBisect verifies bisect session setup and midpoint selection.
func TestIntegrationBisect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	repoPath := filepath.Join(dir, "bisect-test.fossil")

	r, err := repo.Create(repoPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create repo: %v", err)
	}
	defer r.Close()

	// Create 8 sequential checkins.
	rids := make([]libfossil.FslID, 8)
	var parent libfossil.FslID
	for i := 0; i < 8; i++ {
		opts := manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "file.txt", Content: []byte(fmt.Sprintf("v%d\n", i+1))}},
			Comment: fmt.Sprintf("commit %d", i+1),
			User:    "testuser",
			Time:    time.Date(2024, 1, 1+i, 0, 0, 0, 0, time.UTC),
		}
		if parent != 0 {
			opts.Parent = parent
		}
		rids[i], _, err = manifest.Checkin(r, opts)
		if err != nil {
			t.Fatalf("Checkin %d: %v", i+1, err)
		}
		parent = rids[i]
	}

	// Bisect needs vvar (for state) and plink (for path) in the same DB.
	// The repo DB has plink from checkins; add vvar for bisect state.
	if _, err := r.DB().Exec(`CREATE TABLE IF NOT EXISTS vvar(name TEXT PRIMARY KEY, value CLOB) WITHOUT ROWID`); err != nil {
		t.Fatalf("create vvar in repo: %v", err)
	}

	sess := bisect.NewSession(r.DB().SqlDB())

	if err := sess.MarkGood(rids[0]); err != nil {
		t.Fatalf("MarkGood: %v", err)
	}
	if err := sess.MarkBad(rids[7]); err != nil {
		t.Fatalf("MarkBad: %v", err)
	}

	mid, err := sess.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	// Midpoint should not be an endpoint.
	if mid == rids[0] {
		t.Fatalf("midpoint should not be the good endpoint")
	}
	if mid == rids[7] {
		t.Fatalf("midpoint should not be the bad endpoint")
	}

	// Midpoint should be one of the intermediate commits.
	found := false
	for _, rid := range rids[1:7] {
		if mid == rid {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("midpoint %d is not an intermediate commit", mid)
	}
}

// TestIntegrationPath verifies shortest path through 3 sequential checkins.
func TestIntegrationPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	repoPath := filepath.Join(dir, "path-test.fossil")

	r, err := repo.Create(repoPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create repo: %v", err)
	}
	defer r.Close()

	// Create 3 sequential checkins.
	rids := make([]libfossil.FslID, 3)
	var parent libfossil.FslID
	for i := 0; i < 3; i++ {
		opts := manifest.CheckinOpts{
			Files:   []manifest.File{{Name: "file.txt", Content: []byte(fmt.Sprintf("v%d\n", i+1))}},
			Comment: fmt.Sprintf("commit %d", i+1),
			User:    "testuser",
			Time:    time.Date(2024, 1, 1+i, 0, 0, 0, 0, time.UTC),
		}
		if parent != 0 {
			opts.Parent = parent
		}
		rids[i], _, err = manifest.Checkin(r, opts)
		if err != nil {
			t.Fatalf("Checkin %d: %v", i+1, err)
		}
		parent = rids[i]
	}

	p, err := path.Shortest(r.DB().SqlDB(), rids[0], rids[2], false, nil)
	if err != nil {
		t.Fatalf("path.Shortest: %v", err)
	}

	if len(p) != 3 {
		t.Fatalf("path length = %d, want 3", len(p))
	}

	// Verify endpoints.
	if p[0].RID != rids[0] {
		t.Fatalf("path start = %d, want %d", p[0].RID, rids[0])
	}
	if p[2].RID != rids[2] {
		t.Fatalf("path end = %d, want %d", p[2].RID, rids[2])
	}
}
