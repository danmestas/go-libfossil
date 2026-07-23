package checkout

import (
	"context"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

func contains(s, substr string) bool { return strings.Contains(s, substr) }

func TestExtract(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()

	// Use MemStorage to capture extracted files
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}

	// Use a virtual dir for extraction that works with MemStorage
	co.dir = "/checkout"

	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Verify files were written to MemStorage
	data, err := mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal("hello.txt not found:", err)
	}
	if string(data) != "hello world\n" {
		t.Fatalf("hello.txt = %q, want %q", data, "hello world\n")
	}

	data2, err := mem.ReadFile("/checkout/src/main.go")
	if err != nil {
		t.Fatal("src/main.go not found:", err)
	}
	if string(data2) != "package main\n" {
		t.Fatalf("src/main.go = %q", data2)
	}

	data3, err := mem.ReadFile("/checkout/README.md")
	if err != nil {
		t.Fatal("README.md not found:", err)
	}
	if string(data3) != "# Test\n" {
		t.Fatalf("README.md = %q", data3)
	}
}

func TestExtractDryRun(t *testing.T) {
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

	var callbackFiles []string
	err = co.Extract(rid, ExtractOpts{
		DryRun: true,
		Callback: func(name string, change UpdateChange) error {
			callbackFiles = append(callbackFiles, name)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Files should NOT be in MemStorage
	if _, err := mem.ReadFile("/checkout/hello.txt"); err == nil {
		t.Fatal("DryRun should not write files")
	}

	// But callback should have been called
	if len(callbackFiles) != 3 {
		t.Fatalf("expected 3 callback calls, got %d", len(callbackFiles))
	}
}

func TestExtractForceProtection(t *testing.T) {
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
	co.env = &simio.Env{
		Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{},
	}
	co.dir = "/checkout"

	// Extract files first.
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}

	// Modify hello.txt on disk so it differs from vfile mhash.
	if err := mem.WriteFile(
		"/checkout/hello.txt", []byte("local edit\n"), 0644,
	); err != nil {
		t.Fatal(err)
	}

	// Extract again WITHOUT Force — should fail because hello.txt
	// has local changes.
	err = co.Extract(rid, ExtractOpts{Force: false})
	if err == nil {
		t.Fatal("Extract without Force should fail over modified files")
	}
	if !contains(err.Error(), "local changes") {
		t.Fatalf("error should mention local changes, got: %v", err)
	}

	// Extract with Force=true should succeed.
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatalf("Extract with Force should succeed: %v", err)
	}
}

func TestExtractSetMTime(t *testing.T) {
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

	if err := co.Extract(rid, ExtractOpts{SetMTime: true}); err != nil {
		t.Fatal(err)
	}

	// Verify mtime was set on extracted files.
	info, err := mem.Stat("/checkout/hello.txt")
	if err != nil {
		t.Fatal("stat hello.txt:", err)
	}
	mtime := info.ModTime()
	if mtime.IsZero() {
		t.Fatal("mtime should not be zero when SetMTime is true")
	}
	// The checkin timestamp should match what's in the event table.
	// Just verify it's a valid time (not zero) — the exact value depends
	// on the test repo's fixed timestamp.
	if mtime.Year() < 2020 {
		t.Fatalf("mtime %v looks invalid", mtime)
	}

	// Verify all extracted files got the same mtime.
	info2, _ := mem.Stat("/checkout/src/main.go")
	if !info2.ModTime().Equal(mtime) {
		t.Fatalf("src/main.go mtime %v != hello.txt mtime %v", info2.ModTime(), mtime)
	}
}

func TestExtractSetMTimeFalse(t *testing.T) {
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

	if err := co.Extract(rid, ExtractOpts{SetMTime: false}); err != nil {
		t.Fatal(err)
	}

	// Verify mtime was NOT set (should be zero in MemStorage).
	info, err := mem.Stat("/checkout/hello.txt")
	if err != nil {
		t.Fatal("stat hello.txt:", err)
	}
	if !info.ModTime().IsZero() {
		t.Fatalf("mtime should be zero when SetMTime is false, got %v", info.ModTime())
	}
}

func TestExtractSetMTimeDryRun(t *testing.T) {
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

	// DryRun + SetMTime should not write files or set mtimes.
	if err := co.Extract(rid, ExtractOpts{SetMTime: true, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.ReadFile("/checkout/hello.txt"); err == nil {
		t.Fatal("DryRun should not write files even with SetMTime")
	}
}

func TestExtractObserver(t *testing.T) {
	// Use a recording observer to verify hooks fire
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

	type event struct{ name string }
	var events []event
	obs := &testObserver{
		onExtractStarted: func(ctx context.Context, e ExtractStart) context.Context {
			events = append(events, event{"started"})
			return ctx
		},
		onExtractFileCompleted: func(ctx context.Context, name string, change UpdateChange) {
			events = append(events, event{"file:" + name})
		},
		onExtractCompleted: func(ctx context.Context, e ExtractEnd) {
			events = append(events, event{"completed"})
		},
	}
	co.obs = obs
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	co.Extract(rid, ExtractOpts{})

	if len(events) < 5 { // started + 3 files + completed
		t.Fatalf("expected at least 5 events, got %d: %v", len(events), events)
	}
	if events[0].name != "started" {
		t.Fatalf("first event should be started, got %s", events[0].name)
	}
	if events[len(events)-1].name != "completed" {
		t.Fatalf("last event should be completed, got %s", events[len(events)-1].name)
	}
}
