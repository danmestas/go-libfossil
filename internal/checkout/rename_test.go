package checkout

import (
	"database/sql"
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

func TestRename(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Setup MemStorage and extract
	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Rename hello.txt to greet.txt
	err = co.Rename(RenameOpts{
		From: "hello.txt",
		To:   "greet.txt",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify vfile.pathname updated to greet.txt
	var pathname, origname sql.NullString
	var chnged int
	err = co.db.QueryRow("SELECT pathname, origname, chnged FROM vfile WHERE vid=? AND pathname='greet.txt'", int64(rid)).Scan(&pathname, &origname, &chnged)
	if err != nil {
		t.Fatal("vfile row not found:", err)
	}
	if pathname.String != "greet.txt" {
		t.Fatalf("pathname = %s, want greet.txt", pathname.String)
	}
	if !origname.Valid || origname.String != "hello.txt" {
		t.Fatalf("origname = %v, want hello.txt", origname)
	}
	if chnged != 1 {
		t.Fatalf("chnged = %d, want 1", chnged)
	}
}

func TestRenameWithFsMove(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Setup MemStorage and extract
	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Rename hello.txt to greet.txt with filesystem move
	err = co.Rename(RenameOpts{
		From:     "hello.txt",
		To:       "greet.txt",
		DoFsMove: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify old file is gone from MemStorage
	if _, err := mem.Stat("/checkout/hello.txt"); err == nil {
		t.Fatal("old file hello.txt still exists in MemStorage")
	}

	// Verify new file exists in MemStorage
	data, err := mem.ReadFile("/checkout/greet.txt")
	if err != nil {
		t.Fatal("new file greet.txt not found in MemStorage:", err)
	}
	if string(data) != "hello world\n" {
		t.Fatalf("file content = %q, want %q", string(data), "hello world\n")
	}
}

func TestRenameDuplicateTarget(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Setup MemStorage and extract
	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Add a second file
	if err := mem.WriteFile("/checkout/other.txt", []byte("other"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Manage(ManageOpts{Paths: []string{"other.txt"}}); err != nil {
		t.Fatal(err)
	}

	// Try to rename hello.txt to other.txt (should fail)
	err = co.Rename(RenameOpts{
		From: "hello.txt",
		To:   "other.txt",
	})
	if err == nil {
		t.Fatal("expected error when renaming to existing file, got nil")
	}
}

func TestRevertRename(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Setup MemStorage and extract
	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Rename hello.txt to greet.txt
	err = co.Rename(RenameOpts{
		From: "hello.txt",
		To:   "greet.txt",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Revert the rename
	reverted, err := co.RevertRename("greet.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if !reverted {
		t.Fatal("expected reverted=true, got false")
	}

	// Verify vfile.pathname restored to hello.txt
	var pathname string
	var origname sql.NullString
	var chnged int
	err = co.db.QueryRow("SELECT pathname, origname, chnged FROM vfile WHERE vid=? AND pathname='hello.txt'", int64(rid)).Scan(&pathname, &origname, &chnged)
	if err != nil {
		t.Fatal("vfile row not found:", err)
	}
	if pathname != "hello.txt" {
		t.Fatalf("pathname = %s, want hello.txt", pathname)
	}
	if origname.Valid {
		t.Fatalf("origname should be NULL, got %v", origname)
	}
	if chnged != 0 {
		t.Fatalf("chnged = %d, want 0", chnged)
	}
}

func TestRevertRenameNotRenamed(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Setup MemStorage and extract
	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Try to revert a file that hasn't been renamed
	reverted, err := co.RevertRename("hello.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if reverted {
		t.Fatal("expected reverted=false for non-renamed file, got true")
	}
}

func TestRenameWithCallback(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Setup MemStorage and extract
	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Rename with callback
	var callbackFrom, callbackTo string
	err = co.Rename(RenameOpts{
		From: "hello.txt",
		To:   "greet.txt",
		Callback: func(from, to string) error {
			callbackFrom = from
			callbackTo = to
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify callback was called
	if callbackFrom != "hello.txt" {
		t.Fatalf("callback from = %s, want hello.txt", callbackFrom)
	}
	if callbackTo != "greet.txt" {
		t.Fatalf("callback to = %s, want greet.txt", callbackTo)
	}
}

func TestRevertRenameWithFsMove(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Setup MemStorage and extract
	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Rename with filesystem move
	err = co.Rename(RenameOpts{
		From:     "hello.txt",
		To:       "greet.txt",
		DoFsMove: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Revert with filesystem move
	reverted, err := co.RevertRename("greet.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	if !reverted {
		t.Fatal("expected reverted=true, got false")
	}

	// Verify new file is gone from MemStorage
	if _, err := mem.Stat("/checkout/greet.txt"); err == nil {
		t.Fatal("renamed file greet.txt still exists in MemStorage")
	}

	// Verify original file exists in MemStorage
	data, err := mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal("original file hello.txt not found in MemStorage:", err)
	}
	if string(data) != "hello world\n" {
		t.Fatalf("file content = %q, want %q", string(data), "hello world\n")
	}
}
