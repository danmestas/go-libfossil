package checkout

import (
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

func TestRevertModified(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract files
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Modify hello.txt in MemStorage
	if err := mem.WriteFile("/checkout/hello.txt", []byte("modified content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Scan to detect the change (with hash flag to detect content changes)
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}

	// Verify it's marked as changed
	var chnged int
	err = co.db.QueryRow("SELECT chnged FROM vfile WHERE vid=? AND pathname='hello.txt'", int64(rid)).Scan(&chnged)
	if err != nil {
		t.Fatal("vfile row not found:", err)
	}
	if chnged == 0 {
		t.Fatal("expected chnged > 0 after modification")
	}

	// Revert the file
	err = co.Revert(RevertOpts{Paths: []string{"hello.txt"}})
	if err != nil {
		t.Fatal(err)
	}

	// Verify content is restored
	data, err := mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal("file not found after revert:", err)
	}
	if string(data) != "hello world\n" {
		t.Fatalf("content = %q, want %q", data, "hello world\n")
	}

	// Verify vfile.chnged reset to 0
	err = co.db.QueryRow("SELECT chnged FROM vfile WHERE vid=? AND pathname='hello.txt'", int64(rid)).Scan(&chnged)
	if err != nil {
		t.Fatal(err)
	}
	if chnged != 0 {
		t.Fatalf("chnged = %d, want 0 after revert", chnged)
	}
}

func TestRevertDeleted(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract files
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Unmanage hello.txt (marks deleted=1)
	err = co.Unmanage(UnmanageOpts{Paths: []string{"hello.txt"}})
	if err != nil {
		t.Fatal(err)
	}

	// Verify deleted=1
	var deleted int
	err = co.db.QueryRow("SELECT deleted FROM vfile WHERE vid=? AND pathname='hello.txt'", int64(rid)).Scan(&deleted)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 after unmanage", deleted)
	}

	// Revert the file
	err = co.Revert(RevertOpts{Paths: []string{"hello.txt"}})
	if err != nil {
		t.Fatal(err)
	}

	// Verify deleted reset to 0
	err = co.db.QueryRow("SELECT deleted FROM vfile WHERE vid=? AND pathname='hello.txt'", int64(rid)).Scan(&deleted)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 after revert", deleted)
	}

	// Verify content is restored
	data, err := mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal("file not found after revert:", err)
	}
	if string(data) != "hello world\n" {
		t.Fatalf("content = %q, want %q", data, "hello world\n")
	}
}

func TestRevertNewlyAdded(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract files
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Add a new file
	if err := mem.WriteFile("/checkout/new.txt", []byte("new file"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Manage(ManageOpts{Paths: []string{"new.txt"}}); err != nil {
		t.Fatal(err)
	}

	// Verify vfile row exists with rid=0
	var vRid int
	err = co.db.QueryRow("SELECT rid FROM vfile WHERE vid=? AND pathname='new.txt'", int64(rid)).Scan(&vRid)
	if err != nil {
		t.Fatal("vfile row not found:", err)
	}
	if vRid != 0 {
		t.Fatalf("rid = %d, want 0 (newly added)", vRid)
	}

	// Revert the newly added file
	err = co.Revert(RevertOpts{Paths: []string{"new.txt"}})
	if err != nil {
		t.Fatal(err)
	}

	// Verify vfile row is deleted
	var count int
	err = co.db.QueryRow("SELECT count(*) FROM vfile WHERE vid=? AND pathname='new.txt'", int64(rid)).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows after revert, got %d", count)
	}

	// Verify file is removed from Storage (or at least not an error if it doesn't exist)
	_, err = mem.ReadFile("/checkout/new.txt")
	if err == nil {
		t.Fatal("file should be removed after revert")
	}
}

func TestRevertAll(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract files
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Modify hello.txt
	if err := mem.WriteFile("/checkout/hello.txt", []byte("modified 1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Modify src/main.go
	if err := mem.WriteFile("/checkout/src/main.go", []byte("modified 2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Add a new file
	if err := mem.WriteFile("/checkout/new.txt", []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Manage(ManageOpts{Paths: []string{"new.txt"}}); err != nil {
		t.Fatal(err)
	}

	// Scan to detect changes (with hash flag to detect content changes)
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}

	// Verify we have changes
	hasChanges, err := co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if !hasChanges {
		t.Fatal("expected changes before revert")
	}

	// Revert all (empty Paths)
	err = co.Revert(RevertOpts{})
	if err != nil {
		t.Fatal(err)
	}

	// Verify all files are restored
	data1, err := mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal("hello.txt not found after revert:", err)
	}
	if string(data1) != "hello world\n" {
		t.Fatalf("hello.txt = %q, want %q", data1, "hello world\n")
	}

	data2, err := mem.ReadFile("/checkout/src/main.go")
	if err != nil {
		t.Fatal("src/main.go not found after revert:", err)
	}
	if string(data2) != "package main\n" {
		t.Fatalf("src/main.go = %q, want %q", data2, "package main\n")
	}

	// Verify new.txt is removed
	_, err = mem.ReadFile("/checkout/new.txt")
	if err == nil {
		t.Fatal("new.txt should be removed after revert")
	}

	// Verify no changes remain
	hasChanges, err = co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if hasChanges {
		t.Fatal("expected no changes after revert all")
	}
}

func TestRevertCallback(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract and modify a file
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/checkout/hello.txt", []byte("modified"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}

	// Revert with callback
	var callbackName string
	var callbackChange RevertChange
	err = co.Revert(RevertOpts{
		Paths: []string{"hello.txt"},
		Callback: func(name string, change RevertChange) error {
			callbackName = name
			callbackChange = change
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if callbackName != "hello.txt" {
		t.Fatalf("callback name = %q, want %q", callbackName, "hello.txt")
	}
	if callbackChange != RevertContents {
		t.Fatalf("callback change = %v, want RevertContents", callbackChange)
	}
}

func TestRevertCallbackUnmanage(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract, add a new file, then revert it
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/checkout/new.txt", []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Manage(ManageOpts{Paths: []string{"new.txt"}}); err != nil {
		t.Fatal(err)
	}

	// Revert with callback
	var callbackName string
	var callbackChange RevertChange
	err = co.Revert(RevertOpts{
		Paths: []string{"new.txt"},
		Callback: func(name string, change RevertChange) error {
			callbackName = name
			callbackChange = change
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if callbackName != "new.txt" {
		t.Fatalf("callback name = %q, want %q", callbackName, "new.txt")
	}
	if callbackChange != RevertUnmanage {
		t.Fatalf("callback change = %v, want RevertUnmanage", callbackChange)
	}
}

func TestRevertNotFound(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract files
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Revert a file that doesn't exist — should not error (no-op)
	err = co.Revert(RevertOpts{Paths: []string{"nonexistent.txt"}})
	if err != nil {
		t.Fatal("expected no error for nonexistent file, got:", err)
	}
}

func TestRevertNoChanges(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract files but don't modify them
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Revert a clean file — should not error (no-op)
	err = co.Revert(RevertOpts{Paths: []string{"hello.txt"}})
	if err != nil {
		t.Fatal("expected no error for clean file, got:", err)
	}
}
