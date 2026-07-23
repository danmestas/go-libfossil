package checkout

import (
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/simio"
)

func TestManageNewFile(t *testing.T) {
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
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Write a new file to MemStorage
	if err := mem.WriteFile("/checkout/new.txt", []byte("new file"), 0644); err != nil {
		t.Fatal(err)
	}

	counts, err := co.Manage(ManageOpts{Paths: []string{"new.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Added != 1 {
		t.Fatalf("added = %d, want 1", counts.Added)
	}

	// Verify vfile row exists with rid=0, chnged=1
	var vRid, chnged int
	err = co.db.QueryRow("SELECT rid, chnged FROM vfile WHERE vid=? AND pathname='new.txt'", int64(rid)).Scan(&vRid, &chnged)
	if err != nil {
		t.Fatal("vfile row not found:", err)
	}
	if vRid != 0 {
		t.Fatalf("rid = %d, want 0 (newly added)", vRid)
	}
	if chnged != 1 {
		t.Fatalf("chnged = %d, want 1", chnged)
	}
}

func TestManageDuplicate(t *testing.T) {
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
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Try to manage an already-tracked file
	counts, err := co.Manage(ManageOpts{Paths: []string{"hello.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", counts.Skipped)
	}
}

func TestManageCallback(t *testing.T) {
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
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Write a new file
	if err := mem.WriteFile("/checkout/new.txt", []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}

	var called bool
	var addedFlag bool
	_, err = co.Manage(ManageOpts{
		Paths: []string{"new.txt"},
		Callback: func(name string, added bool) error {
			called = true
			addedFlag = added
			if name != "new.txt" {
				t.Fatalf("callback name = %q, want %q", name, "new.txt")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !called {
		t.Fatal("callback not called")
	}
	if !addedFlag {
		t.Fatal("callback added flag should be true")
	}
}

func TestManageCallbackSkipped(t *testing.T) {
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
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	var called bool
	var addedFlag bool
	_, err = co.Manage(ManageOpts{
		Paths: []string{"hello.txt"},
		Callback: func(name string, added bool) error {
			called = true
			addedFlag = added
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !called {
		t.Fatal("callback not called")
	}
	if addedFlag {
		t.Fatal("callback added flag should be false for skipped file")
	}
}

func TestUnmanageNewFile(t *testing.T) {
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
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Add then remove a new file
	if err := mem.WriteFile("/checkout/new.txt", []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Manage(ManageOpts{Paths: []string{"new.txt"}}); err != nil {
		t.Fatal(err)
	}

	err = co.Unmanage(UnmanageOpts{Paths: []string{"new.txt"}})
	if err != nil {
		t.Fatal(err)
	}

	// Should be completely removed (rid=0 -> DELETE)
	var count int
	err = co.db.QueryRow("SELECT count(*) FROM vfile WHERE vid=? AND pathname='new.txt'", int64(rid)).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows for unmanaged new file, got %d", count)
	}
}

func TestUnmanageExistingFile(t *testing.T) {
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
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Unmanage an existing tracked file
	err = co.Unmanage(UnmanageOpts{Paths: []string{"hello.txt"}})
	if err != nil {
		t.Fatal(err)
	}

	// Should be marked deleted=1 (not removed, because rid>0)
	var deleted int
	err = co.db.QueryRow("SELECT deleted FROM vfile WHERE vid=? AND pathname='hello.txt'", int64(rid)).Scan(&deleted)
	if err != nil {
		t.Fatal("vfile row not found:", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
}

func TestUnmanageCallback(t *testing.T) {
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
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	var called bool
	var calledName string
	err = co.Unmanage(UnmanageOpts{
		Paths: []string{"hello.txt"},
		Callback: func(name string) error {
			called = true
			calledName = name
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !called {
		t.Fatal("callback not called")
	}
	if calledName != "hello.txt" {
		t.Fatalf("callback name = %q, want %q", calledName, "hello.txt")
	}
}

func TestUnmanageNotFound(t *testing.T) {
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
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Unmanage a file that doesn't exist — should not error
	err = co.Unmanage(UnmanageOpts{Paths: []string{"nonexistent.txt"}})
	if err != nil {
		t.Fatal("expected no error for nonexistent file, got:", err)
	}
}

func TestUnmanageByVFileID(t *testing.T) {
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
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Get vfile ID for hello.txt
	var vfileID int64
	err = co.db.QueryRow("SELECT id FROM vfile WHERE vid=? AND pathname='hello.txt'", int64(rid)).Scan(&vfileID)
	if err != nil {
		t.Fatal(err)
	}

	// Unmanage by VFileID
	err = co.Unmanage(UnmanageOpts{VFileIDs: []libfossil.FslID{libfossil.FslID(vfileID)}})
	if err != nil {
		t.Fatal(err)
	}

	// Should be marked deleted=1
	var deleted int
	err = co.db.QueryRow("SELECT deleted FROM vfile WHERE id=?", vfileID).Scan(&deleted)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
}
