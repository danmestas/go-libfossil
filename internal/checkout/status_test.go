package checkout

import (
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

func TestHasChangesClean(t *testing.T) {
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

	has, err := co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("clean checkout should not have changes")
	}
}

func TestHasChangesModified(t *testing.T) {
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

	// Modify file and scan
	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}

	has, err := co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("should have changes after modification")
	}
}

func TestVisitChanges(t *testing.T) {
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

	// Modify a file
	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// VisitChanges with scan=true should detect the change
	var entries []ChangeEntry
	err = co.VisitChanges(rid, true, func(e ChangeEntry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 change, got %d", len(entries))
	}
	if entries[0].Name != "hello.txt" {
		t.Fatalf("changed file = %q, want hello.txt", entries[0].Name)
	}
	if entries[0].Change != ChangeModified {
		t.Fatalf("change type = %d, want ChangeModified", entries[0].Change)
	}
}

func TestVisitChangesNoScan(t *testing.T) {
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

	// Modify a file but don't scan
	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// VisitChanges with scan=false should NOT detect the change
	var entries []ChangeEntry
	err = co.VisitChanges(rid, false, func(e ChangeEntry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 changes without scan, got %d", len(entries))
	}
}

func TestVisitChangesMultiple(t *testing.T) {
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

	// Modify multiple files
	if err := mem.WriteFile("/checkout/hello.txt", []byte("changed1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteFile("/checkout/README.md", []byte("changed2"), 0644); err != nil {
		t.Fatal(err)
	}

	// VisitChanges should detect both changes
	var entries []ChangeEntry
	err = co.VisitChanges(rid, true, func(e ChangeEntry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(entries))
	}

	// Check that both files are present
	found := make(map[string]bool)
	for _, e := range entries {
		if e.Change != ChangeModified {
			t.Errorf("file %s: expected ChangeModified, got %d", e.Name, e.Change)
		}
		found[e.Name] = true
	}
	if !found["hello.txt"] || !found["README.md"] {
		t.Fatal("expected hello.txt and README.md to be in changes")
	}
}
