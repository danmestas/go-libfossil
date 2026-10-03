package checkout

import (
	"context"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
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

// newCheckoutAtFirstOfTwo builds a repo with two checkins (see
// newTestRepoWithTwoCheckins: rid2 edits hello.txt and adds new.txt) and a
// checkout with rid1 extracted into memory storage.
func newCheckoutAtFirstOfTwo(t *testing.T) (co *Checkout, mem *simio.MemStorage, rid1, rid2 libfossil.FslID) {
	t.Helper()
	r, rid1, rid2, cleanup := newTestRepoWithTwoCheckins(t)
	t.Cleanup(cleanup)

	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	if err := setVVar(co.db, "checkout", itoa(int64(rid1))); err != nil {
		t.Fatal(err)
	}
	mem = simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatal("extract rid1:", err)
	}
	return co, mem, rid1, rid2
}

// changedNames scans the checkout's current version and returns the names
// of its changed files.
func changedNames(t *testing.T, co *Checkout) []string {
	t.Helper()
	vid, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	err = co.VisitChanges(vid, true, func(e ChangeEntry) error {
		names = append(names, e.Name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// Switching a clean checkout to another version is what Extract is for: the
// files that differ between the versions are not local changes (#230).
func TestExtractSwitchesCleanCheckout(t *testing.T) {
	co, mem, _, rid2 := newCheckoutAtFirstOfTwo(t)

	if err := co.Extract(rid2, ExtractOpts{}); err != nil {
		t.Fatalf("Extract to the next version of a clean checkout: %v", err)
	}
	data, err := mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello updated world\n" {
		t.Fatalf("hello.txt = %q, want rid2's content", data)
	}
	if vid, _, _ := co.Version(); vid != rid2 {
		t.Fatalf("Version = %d, want %d", vid, rid2)
	}
}

// A refused Extract must leave the checkout exactly as it was: same version,
// and the unsaved edit still reported (#230).
func TestExtractRefusalLeavesCheckoutUntouched(t *testing.T) {
	co, mem, rid1, rid2 := newCheckoutAtFirstOfTwo(t)
	if err := mem.WriteFile("/checkout/README.md", []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := co.Extract(rid2, ExtractOpts{})
	if err == nil {
		t.Fatal("Extract over an unsaved edit succeeded")
	}
	if !contains(err.Error(), "local changes") {
		t.Fatalf("error should mention local changes, got: %v", err)
	}
	if vid, _, _ := co.Version(); vid != rid1 {
		t.Fatalf("Version = %d after refusal, want %d", vid, rid1)
	}
	if got := changedNames(t, co); len(got) != 1 || got[0] != "README.md" {
		t.Fatalf("changes after refusal = %q, want only README.md", got)
	}
}

// A pending add is unsaved work too, even though the target version would
// not write over the file.
func TestExtractRefusesOverPendingAdd(t *testing.T) {
	co, mem, _, rid2 := newCheckoutAtFirstOfTwo(t)
	if err := mem.WriteFile("/checkout/extra.txt", []byte("extra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Manage(ManageOpts{Paths: []string{"extra.txt"}}); err != nil {
		t.Fatal(err)
	}

	if err := co.Extract(rid2, ExtractOpts{}); err == nil {
		t.Fatal("Extract over a pending add succeeded")
	}
	if got := changedNames(t, co); len(got) != 1 || got[0] != "extra.txt" {
		t.Fatalf("changes after refusal = %q, want only extra.txt", got)
	}
}

// A file the current version does not track, sitting where the target would
// write different content, is the user's and must not be clobbered.
func TestExtractRefusesOverUntrackedFile(t *testing.T) {
	co, mem, _, rid2 := newCheckoutAtFirstOfTwo(t)
	if err := mem.WriteFile("/checkout/new.txt", []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := co.Extract(rid2, ExtractOpts{}); err == nil {
		t.Fatal("Extract over an untracked file succeeded")
	}
	data, err := mem.ReadFile("/checkout/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "mine\n" {
		t.Fatalf("new.txt = %q, untracked file was overwritten", data)
	}
}

// Create records the tip as checked out before any file is written. Extracting
// an older version into a directory that already holds it must still work:
// the files differ from the tip, but none would lose content. This is how
// EdgeSync's ExtractTo uses Create + Extract.
func TestExtractReextractOlderVersionIntoCreatedCheckout(t *testing.T) {
	r, rid1, _, cleanup := newTestRepoWithTwoCheckins(t)
	defer cleanup()
	mem := simio.NewMemStorage()
	env := &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}

	for pass := 1; pass <= 2; pass++ {
		co, err := Create(r, t.TempDir(), CreateOpts{})
		if err != nil {
			t.Fatal(err)
		}
		co.env = env
		co.dir = "/checkout"
		err = co.Extract(rid1, ExtractOpts{})
		co.Close()
		if err != nil {
			t.Fatalf("pass %d: Extract(rid1) into a created checkout: %v", pass, err)
		}
	}
	data, err := mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world\n" {
		t.Fatalf("hello.txt = %q, want rid1's content", data)
	}
}
