package checkout

import (
	"context"
	"strconv"
	"testing"
	"time"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
)

// newTestRepoWithTwoCheckins creates a repo with two checkins.
// First checkin: hello.txt, src/main.go, README.md
// Second checkin: hello.txt modified, src/main.go unchanged, README.md unchanged, new.txt added
// Returns repo, rid1, rid2, cleanup.
func newTestRepoWithTwoCheckins(t *testing.T) (*repo.Repo, libfossil.FslID, libfossil.FslID, func()) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/test.fossil"
	r, err := repo.CreateWithEnv(path, "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}

	// First checkin
	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "hello.txt", Content: []byte("hello world\n")},
			{Name: "src/main.go", Content: []byte("package main\n")},
			{Name: "README.md", Content: []byte("# Test\n")},
		},
		Comment: "initial checkin",
		User:    "test",
		Parent:  0,
		Time:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		r.Close()
		t.Fatal(err)
	}

	// Second checkin — modify hello.txt, add new.txt
	rid2, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "hello.txt", Content: []byte("hello updated world\n")},
			{Name: "src/main.go", Content: []byte("package main\n")},
			{Name: "README.md", Content: []byte("# Test\n")},
			{Name: "new.txt", Content: []byte("new file\n")},
		},
		Comment: "second checkin",
		User:    "test",
		Parent:  rid1,
		Time:    time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		r.Close()
		t.Fatal(err)
	}

	return r, rid1, rid2, func() { r.Close() }
}

func TestCalcUpdateVersion(t *testing.T) {
	r, rid1, rid2, cleanup := newTestRepoWithTwoCheckins(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Checkout is at tip (rid2) after Create — CalcUpdateVersion should return 0.
	tip, err := co.CalcUpdateVersion()
	if err != nil {
		t.Fatal(err)
	}
	if tip != 0 {
		t.Fatalf("expected CalcUpdateVersion=0 at tip, got %d", tip)
	}

	// Set checkout to rid1 (the older version)
	if err := setVVar(co.db, "checkout", itoa(int64(rid1))); err != nil {
		t.Fatal(err)
	}
	var uuid1 string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid1).Scan(&uuid1); err != nil {
		t.Fatal(err)
	}
	if err := setVVar(co.db, "checkout-hash", uuid1); err != nil {
		t.Fatal(err)
	}

	// Now CalcUpdateVersion should return rid2
	tip, err = co.CalcUpdateVersion()
	if err != nil {
		t.Fatal(err)
	}
	if tip != rid2 {
		t.Fatalf("CalcUpdateVersion = %d, want %d", tip, rid2)
	}
}

func TestUpdateLinear(t *testing.T) {
	r, rid1, rid2, cleanup := newTestRepoWithTwoCheckins(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Set checkout to rid1
	if err := setVVar(co.db, "checkout", itoa(int64(rid1))); err != nil {
		t.Fatal(err)
	}
	var uuid1 string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid1).Scan(&uuid1); err != nil {
		t.Fatal(err)
	}
	if err := setVVar(co.db, "checkout-hash", uuid1); err != nil {
		t.Fatal(err)
	}

	// Extract rid1 files to MemStorage
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatal("extract rid1:", err)
	}

	// Verify initial state
	data, err := mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world\n" {
		t.Fatalf("before update: hello.txt = %q", data)
	}

	// Track changes via callback
	var changes []struct {
		name   string
		change UpdateChange
	}
	err = co.Update(UpdateOpts{
		TargetRID: rid2,
		Callback: func(name string, change UpdateChange) error {
			changes = append(changes, struct {
				name   string
				change UpdateChange
			}{name, change})
			return nil
		},
	})
	if err != nil {
		t.Fatal("Update:", err)
	}

	// Verify hello.txt was updated
	data, err = mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello updated world\n" {
		t.Fatalf("after update: hello.txt = %q", data)
	}

	// Verify new.txt was added
	data, err = mem.ReadFile("/checkout/new.txt")
	if err != nil {
		t.Fatal("new.txt not found:", err)
	}
	if string(data) != "new file\n" {
		t.Fatalf("new.txt = %q", data)
	}

	// Verify unchanged files still present
	data, err = mem.ReadFile("/checkout/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package main\n" {
		t.Fatalf("src/main.go = %q", data)
	}

	// Verify checkout version updated
	curRID, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if curRID != rid2 {
		t.Fatalf("after update: version RID = %d, want %d", curRID, rid2)
	}

	// Verify callbacks fired for changed files
	if len(changes) < 2 {
		t.Fatalf("expected at least 2 change callbacks, got %d", len(changes))
	}

	// Check that we got an UpdateUpdated for hello.txt and UpdateAdded for new.txt
	foundUpdated := false
	foundAdded := false
	for _, ch := range changes {
		if ch.name == "hello.txt" && ch.change == UpdateUpdated {
			foundUpdated = true
		}
		if ch.name == "new.txt" && ch.change == UpdateAdded {
			foundAdded = true
		}
	}
	if !foundUpdated {
		t.Error("expected UpdateUpdated callback for hello.txt")
	}
	if !foundAdded {
		t.Error("expected UpdateAdded callback for new.txt")
	}
}

func TestUpdateNoChanges(t *testing.T) {
	r, _, _, cleanup := newTestRepoWithTwoCheckins(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Checkout is already at tip — Update should be a no-op
	err = co.Update(UpdateOpts{})
	if err != nil {
		t.Fatal("Update at tip should succeed:", err)
	}
}

func TestUpdateObserver(t *testing.T) {
	r, rid1, rid2, cleanup := newTestRepoWithTwoCheckins(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Set to rid1
	if err := setVVar(co.db, "checkout", itoa(int64(rid1))); err != nil {
		t.Fatal(err)
	}
	var uuid1 string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid1).Scan(&uuid1); err != nil {
		t.Fatal(err)
	}
	if err := setVVar(co.db, "checkout-hash", uuid1); err != nil {
		t.Fatal(err)
	}

	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract first so files exist
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Install recording observer
	type event struct{ name string }
	var events []event
	obs := &testObserver{
		onExtractStarted: func(ctx context.Context, e ExtractStart) context.Context {
			if e.Operation != "update" {
				t.Errorf("expected operation=update, got %s", e.Operation)
			}
			if e.TargetRID != rid2 {
				t.Errorf("expected target=%d, got %d", rid2, e.TargetRID)
			}
			events = append(events, event{"started"})
			return ctx
		},
		onExtractFileCompleted: func(ctx context.Context, name string, change UpdateChange) {
			events = append(events, event{"file:" + name})
		},
		onExtractCompleted: func(ctx context.Context, e ExtractEnd) {
			if e.Operation != "update" {
				t.Errorf("expected operation=update, got %s", e.Operation)
			}
			events = append(events, event{"completed"})
		},
	}
	co.obs = obs

	if err := co.Update(UpdateOpts{TargetRID: rid2}); err != nil {
		t.Fatal(err)
	}

	if len(events) < 3 { // started + at least 1 file + completed
		t.Fatalf("expected at least 3 events, got %d: %v", len(events), events)
	}
	if events[0].name != "started" {
		t.Fatalf("first event = %s, want started", events[0].name)
	}
	if events[len(events)-1].name != "completed" {
		t.Fatalf("last event = %s, want completed", events[len(events)-1].name)
	}
}

func TestUpdateDryRun(t *testing.T) {
	r, rid1, rid2, cleanup := newTestRepoWithTwoCheckins(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Set to rid1
	if err := setVVar(co.db, "checkout", itoa(int64(rid1))); err != nil {
		t.Fatal(err)
	}
	var uuid1 string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid1).Scan(&uuid1); err != nil {
		t.Fatal(err)
	}
	if err := setVVar(co.db, "checkout-hash", uuid1); err != nil {
		t.Fatal(err)
	}

	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract rid1 files
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// DryRun update — should NOT modify files on disk
	var callbackCount int
	err = co.Update(UpdateOpts{
		TargetRID: rid2,
		DryRun:    true,
		Callback: func(name string, change UpdateChange) error {
			callbackCount++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// hello.txt should still have old content (dry run)
	data, err := mem.ReadFile("/checkout/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world\n" {
		t.Fatalf("dry run should not modify hello.txt, got %q", data)
	}

	// new.txt should NOT exist (dry run)
	if _, err := mem.ReadFile("/checkout/new.txt"); err == nil {
		t.Fatal("dry run should not create new.txt")
	}

	// But callbacks should have fired
	if callbackCount == 0 {
		t.Fatal("expected callbacks during dry run")
	}
}

func TestUpdateWithFileRemoval(t *testing.T) {
	// Create a repo where the second checkin removes a file.
	dir := t.TempDir()
	path := dir + "/test.fossil"
	r, err := repo.CreateWithEnv(path, "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// First checkin: three files.
	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "keep.txt", Content: []byte("keep\n")},
			{Name: "remove.txt", Content: []byte("bye\n")},
		},
		Comment: "initial",
		User:    "test",
		Parent:  0,
		Time:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second checkin: remove.txt omitted.
	rid2, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "keep.txt", Content: []byte("keep\n")},
		},
		Comment: "remove file",
		User:    "test",
		Parent:  rid1,
		Time:    time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create checkout at rid1.
	ckDir := t.TempDir()
	co, err := Create(r, ckDir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Point checkout to rid1.
	if err := setVVar(co.db, "checkout", itoa(int64(rid1))); err != nil {
		t.Fatal(err)
	}
	var uuid1 string
	r.DB().QueryRow(
		"SELECT uuid FROM blob WHERE rid=?", rid1,
	).Scan(&uuid1)
	if err := setVVar(co.db, "checkout-hash", uuid1); err != nil {
		t.Fatal(err)
	}

	mem := simio.NewMemStorage()
	co.env = &simio.Env{
		Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{},
	}
	co.dir = "/checkout"
	if err := co.Extract(rid1, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}

	// Verify remove.txt exists before update.
	if _, err := mem.ReadFile("/checkout/remove.txt"); err != nil {
		t.Fatal("remove.txt should exist before update:", err)
	}

	// Update to rid2.
	if err := co.Update(UpdateOpts{TargetRID: rid2}); err != nil {
		t.Fatal(err)
	}

	// remove.txt should be deleted from Storage.
	if _, err := mem.ReadFile("/checkout/remove.txt"); err == nil {
		t.Fatal("remove.txt should be deleted after update")
	}

	// keep.txt should still exist.
	data, err := mem.ReadFile("/checkout/keep.txt")
	if err != nil {
		t.Fatal("keep.txt not found:", err)
	}
	if string(data) != "keep\n" {
		t.Fatalf("keep.txt = %q, want %q", data, "keep\n")
	}

	_ = rid2 // used above
}

func TestUpdateSetMTime(t *testing.T) {
	r, rid1, rid2, cleanup := newTestRepoWithTwoCheckins(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Set checkout to rid1.
	if err := setVVar(co.db, "checkout", itoa(int64(rid1))); err != nil {
		t.Fatal(err)
	}
	var uuid1 string
	r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid1).Scan(&uuid1)
	if err := setVVar(co.db, "checkout-hash", uuid1); err != nil {
		t.Fatal(err)
	}

	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatal("extract:", err)
	}

	// Update to rid2 with SetMTime.
	if err := co.Update(UpdateOpts{TargetRID: rid2, SetMTime: true}); err != nil {
		t.Fatal("update:", err)
	}

	// hello.txt was modified in rid2 — should have rid2's checkin mtime (2026-01-02).
	info, err := mem.Stat("/checkout/hello.txt")
	if err != nil {
		t.Fatal("stat hello.txt:", err)
	}
	mtime := info.ModTime()
	if mtime.IsZero() {
		t.Fatal("mtime should not be zero when SetMTime is true")
	}
	// rid2 timestamp is 2026-01-02.
	expected := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if !mtime.Equal(expected) {
		t.Fatalf("hello.txt mtime = %v, want %v", mtime, expected)
	}

	// new.txt was added in rid2 — should also have rid2's mtime.
	info2, err := mem.Stat("/checkout/new.txt")
	if err != nil {
		t.Fatal("stat new.txt:", err)
	}
	if !info2.ModTime().Equal(expected) {
		t.Fatalf("new.txt mtime = %v, want %v", info2.ModTime(), expected)
	}
}

func TestUpdateSetMTimeFalse(t *testing.T) {
	r, rid1, rid2, cleanup := newTestRepoWithTwoCheckins(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if err := setVVar(co.db, "checkout", itoa(int64(rid1))); err != nil {
		t.Fatal(err)
	}
	var uuid1 string
	r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid1).Scan(&uuid1)
	setVVar(co.db, "checkout-hash", uuid1)

	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	co.Extract(rid1, ExtractOpts{})

	// Update WITHOUT SetMTime.
	if err := co.Update(UpdateOpts{TargetRID: rid2, SetMTime: false}); err != nil {
		t.Fatal("update:", err)
	}

	info, err := mem.Stat("/checkout/hello.txt")
	if err != nil {
		t.Fatal("stat:", err)
	}
	if !info.ModTime().IsZero() {
		t.Fatalf("mtime should be zero when SetMTime is false, got %v", info.ModTime())
	}
}

// itoa is a helper to avoid importing strconv in tests.
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
