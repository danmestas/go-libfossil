package checkout

import (
	"context"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
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
	result, err := co.Update(UpdateOpts{
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

	// A clean update must report no conflicted paths, and must report the
	// paths it actually touched — not merely how many.
	if len(result.Conflicted) != 0 {
		t.Fatalf("expected no conflicts, got %v", result.Conflicted)
	}
	if !containsPath(result.FilesWritten, "hello.txt") {
		t.Fatalf("expected FilesWritten to contain hello.txt, got %v", result.FilesWritten)
	}
	if !containsPath(result.FilesWritten, "new.txt") {
		t.Fatalf("expected FilesWritten to contain new.txt, got %v", result.FilesWritten)
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
	result, err := co.Update(UpdateOpts{})
	if err != nil {
		t.Fatal("Update at tip should succeed:", err)
	}
	if len(result.Conflicted) != 0 || len(result.FilesWritten) != 0 || len(result.FilesRemoved) != 0 {
		t.Fatalf("expected empty result for no-op update, got %+v", result)
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

	if _, err := co.Update(UpdateOpts{TargetRID: rid2}); err != nil {
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
	_, err = co.Update(UpdateOpts{
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
	result, err := co.Update(UpdateOpts{TargetRID: rid2})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPath(result.FilesRemoved, "remove.txt") {
		t.Fatalf("expected FilesRemoved to contain remove.txt, got %v", result.FilesRemoved)
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
	if _, err := co.Update(UpdateOpts{TargetRID: rid2, SetMTime: true}); err != nil {
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
	if _, err := co.Update(UpdateOpts{TargetRID: rid2, SetMTime: false}); err != nil {
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

// containsPath reports whether paths contains name.
func containsPath(paths []string, name string) bool {
	for _, p := range paths {
		if p == name {
			return true
		}
	}
	return false
}

// TestUpdateResultPathsAreSorted builds an update touching four files whose
// names are deliberately not in the order the underlying map would build
// them (allNames is a map[string]bool; Go randomizes iteration over it).
// Two files conflict (alpha.txt, zeta.txt) and two are clean additions
// (new_a.txt, new_z.txt). Every returned slice must come back sorted
// regardless of iteration order, so a caller can use reflect.DeepEqual or a
// golden file against the result instead of getting an intermittently
// ordered list.
func TestUpdateResultPathsAreSorted(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.fossil"
	r, err := repo.CreateWithEnv(path, "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	ridBase, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "zeta.txt", Content: []byte("original\n")},
			{Name: "alpha.txt", Content: []byte("original\n")},
			{Name: "stable.txt", Content: []byte("stable\n")},
		},
		Comment: "base",
		User:    "test",
		Parent:  0,
		Time:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	ridA, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "zeta.txt", Content: []byte("local version\n")},
			{Name: "alpha.txt", Content: []byte("local version\n")},
			{Name: "stable.txt", Content: []byte("stable\n")},
		},
		Comment: "branch A",
		User:    "test",
		Parent:  ridBase,
		Time:    time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Branch B: conflicting edits to zeta.txt/alpha.txt, plus two brand-new
	// files that will be clean UpdateAdded entries in FilesWritten.
	ridB, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "zeta.txt", Content: []byte("remote version\n")},
			{Name: "alpha.txt", Content: []byte("remote version\n")},
			{Name: "stable.txt", Content: []byte("stable\n")},
			{Name: "new_z.txt", Content: []byte("new\n")},
			{Name: "new_a.txt", Content: []byte("new\n")},
		},
		Comment: "branch B",
		User:    "test",
		Parent:  ridBase,
		Time:    time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	ckDir := t.TempDir()
	co, err := Create(r, ckDir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if err := setVVar(co.db, "checkout", itoa(int64(ridA))); err != nil {
		t.Fatal(err)
	}
	var uuidA string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", ridA).Scan(&uuidA); err != nil {
		t.Fatal(err)
	}
	if err := setVVar(co.db, "checkout-hash", uuidA); err != nil {
		t.Fatal(err)
	}

	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(ridA, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	result, err := co.Update(UpdateOpts{TargetRID: ridB})
	if err != nil {
		t.Fatalf("Update with conflicts should still succeed (err==nil): %v", err)
	}

	wantConflicted := []string{"alpha.txt", "zeta.txt"}
	if !reflect.DeepEqual(result.Conflicted, wantConflicted) {
		t.Fatalf("Conflicted = %v, want sorted %v", result.Conflicted, wantConflicted)
	}

	wantWritten := []string{"alpha.txt", "new_a.txt", "new_z.txt", "zeta.txt"}
	if !reflect.DeepEqual(result.FilesWritten, wantWritten) {
		t.Fatalf("FilesWritten = %v, want sorted %v", result.FilesWritten, wantWritten)
	}

	if !sort.StringsAreSorted(result.FilesWritten) {
		t.Fatalf("FilesWritten not sorted: %v", result.FilesWritten)
	}
	if !sort.StringsAreSorted(result.Conflicted) {
		t.Fatalf("Conflicted not sorted: %v", result.Conflicted)
	}
}

// TestUpdateConflictSurfacesPaths builds a genuine fork — two checkins with
// the same parent, both editing the same line of the same file — and updates
// across it. The three-way merge cannot resolve the overlapping edit cleanly,
// so it writes conflict markers into conflict.txt. The update must still
// report err == nil (this is a successful update — the working tree is
// usable, just not clean) and must list conflict.txt in Conflicted.
func TestUpdateConflictSurfacesPaths(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.fossil"
	r, err := repo.CreateWithEnv(path, "test", simio.RealEnv(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Ancestor: base content both sides will diverge from.
	ridBase, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "conflict.txt", Content: []byte("original\n")},
			{Name: "stable.txt", Content: []byte("stable\n")},
		},
		Comment: "base",
		User:    "test",
		Parent:  0,
		Time:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Leaf A ("current"): edits conflict.txt one way.
	ridA, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "conflict.txt", Content: []byte("local version\n")},
			{Name: "stable.txt", Content: []byte("stable\n")},
		},
		Comment: "branch A",
		User:    "test",
		Parent:  ridBase,
		Time:    time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Leaf B ("target"): edits conflict.txt a different, incompatible way.
	ridB, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "conflict.txt", Content: []byte("remote version\n")},
			{Name: "stable.txt", Content: []byte("stable\n")},
		},
		Comment: "branch B",
		User:    "test",
		Parent:  ridBase,
		Time:    time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	ckDir := t.TempDir()
	co, err := Create(r, ckDir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Point the checkout at leaf A.
	if err := setVVar(co.db, "checkout", itoa(int64(ridA))); err != nil {
		t.Fatal(err)
	}
	var uuidA string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", ridA).Scan(&uuidA); err != nil {
		t.Fatal(err)
	}
	if err := setVVar(co.db, "checkout-hash", uuidA); err != nil {
		t.Fatal(err)
	}

	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"
	if err := co.Extract(ridA, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Update from leaf A to leaf B — this must 3-way merge conflict.txt
	// against the common ancestor (ridBase) and fail to merge cleanly.
	result, err := co.Update(UpdateOpts{TargetRID: ridB})
	if err != nil {
		t.Fatalf("Update with conflict should still succeed (err==nil): %v", err)
	}

	if len(result.Conflicted) != 1 || result.Conflicted[0] != "conflict.txt" {
		t.Fatalf("expected Conflicted=[conflict.txt], got %v", result.Conflicted)
	}
	if !containsPath(result.FilesWritten, "conflict.txt") {
		t.Fatalf("expected FilesWritten to contain conflict.txt, got %v", result.FilesWritten)
	}

	// The conflicted file must actually contain conflict markers on disk —
	// that's the entire point of the Conflicted field.
	data, err := mem.ReadFile("/checkout/conflict.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<<<<<<<") {
		t.Fatalf("expected conflict markers in conflict.txt, got %q", data)
	}
}

// TestUpdateFailureIsDistinguishableFromConflict confirms a genuine failure
// (target checkin does not exist) returns a non-nil error, distinct from the
// conflicted-but-successful case above.
func TestUpdateFailureIsDistinguishableFromConflict(t *testing.T) {
	r, rid1, _, cleanup := newTestRepoWithTwoCheckins(t)
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
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid1).Scan(&uuid1); err != nil {
		t.Fatal(err)
	}
	if err := setVVar(co.db, "checkout-hash", uuid1); err != nil {
		t.Fatal(err)
	}

	_, err = co.Update(UpdateOpts{TargetRID: 999999})
	if err == nil {
		t.Fatal("expected error for nonexistent target RID, got nil")
	}
}
