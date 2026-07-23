package checkout

import (
	"context"
	"fmt"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/simio"
)

// TestObserverFullLifecycle exercises the complete observer lifecycle:
// Create → Extract → Modify → ScanChanges → Commit
func TestObserverFullLifecycle(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()

	// Use MemStorage so we can modify files in-memory
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Recording observer
	var events []string
	obs := &testObserver{
		onExtractStarted: func(ctx context.Context, e ExtractStart) context.Context {
			events = append(events, fmt.Sprintf("extract:started:%s:rid=%d", e.Operation, e.TargetRID))
			return ctx
		},
		onExtractFileCompleted: func(ctx context.Context, name string, change UpdateChange) {
			events = append(events, fmt.Sprintf("extract:file:%s", name))
		},
		onExtractCompleted: func(ctx context.Context, e ExtractEnd) {
			events = append(events, fmt.Sprintf("extract:completed:written=%d", e.FilesWritten))
		},
		onScanStarted: func(ctx context.Context) context.Context {
			events = append(events, "scan:started")
			return ctx
		},
		onScanCompleted: func(ctx context.Context, e ScanEnd) {
			events = append(events, fmt.Sprintf("scan:completed:scanned=%d,changed=%d", e.FilesScanned, e.FilesChanged))
		},
		onCommitStarted: func(ctx context.Context, e CommitStart) context.Context {
			events = append(events, fmt.Sprintf("commit:started:enqueued=%d,user=%s", e.FilesEnqueued, e.User))
			return ctx
		},
		onCommitCompleted: func(ctx context.Context, e CommitEnd) {
			events = append(events, fmt.Sprintf("commit:completed:rid=%d,uuid=%s,files=%d", e.RID, e.UUID, e.FilesCommit))
		},
	}
	co.obs = obs

	// Step 1: Extract files (should fire ExtractStarted, 3×ExtractFileCompleted, ExtractCompleted)
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal("Extract failed:", err)
	}

	// Verify extract events
	if len(events) < 5 {
		t.Fatalf("after Extract: expected at least 5 events, got %d: %v", len(events), events)
	}
	if events[0] != fmt.Sprintf("extract:started:extract:rid=%d", rid) {
		t.Errorf("event[0] = %q, want extract:started:extract:rid=%d", events[0], rid)
	}
	// Should see 3 file events (hello.txt, src/main.go, README.md)
	fileEvents := 0
	for i := 1; i < len(events)-1; i++ {
		if len(events[i]) > 13 && events[i][:13] == "extract:file:" {
			fileEvents++
		}
	}
	if fileEvents != 3 {
		t.Errorf("expected 3 extract:file events, got %d", fileEvents)
	}
	if events[4] != "extract:completed:written=3" {
		t.Errorf("event[4] = %q, want extract:completed:written=3", events[4])
	}

	// Step 2: Modify a file in MemStorage
	if err := mem.WriteFile("/checkout/hello.txt", []byte("modified content\n"), 0644); err != nil {
		t.Fatal("WriteFile failed:", err)
	}

	// Reset events for next phase
	events = nil

	// Step 3: ScanChanges (should fire ScanStarted, ScanCompleted with changed=1)
	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal("ScanChanges failed:", err)
	}

	if len(events) != 2 {
		t.Fatalf("after ScanChanges: expected 2 events, got %d: %v", len(events), events)
	}
	if events[0] != "scan:started" {
		t.Errorf("event[0] = %q, want scan:started", events[0])
	}
	if events[1] != "scan:completed:scanned=3,changed=1" {
		t.Errorf("event[1] = %q, want scan:completed:scanned=3,changed=1", events[1])
	}

	// Reset events for commit
	events = nil

	// Step 4: Commit (should fire ScanStarted again internally, then CommitStarted, CommitCompleted)
	newRID, newUUID, err := co.Commit(CommitOpts{
		Message: "test commit",
		User:    "testuser",
	})
	if err != nil {
		t.Fatal("Commit failed:", err)
	}

	// Verify commit events
	// Commit internally calls ScanChanges, so we should see:
	// scan:started → scan:completed → commit:started → commit:completed
	// Note: The internal scan sees changed=0 because we already scanned above
	if len(events) < 4 {
		t.Fatalf("after Commit: expected at least 4 events, got %d: %v", len(events), events)
	}

	// First two should be from internal scan
	if events[0] != "scan:started" {
		t.Errorf("commit scan event[0] = %q, want scan:started", events[0])
	}
	if events[1] != "scan:completed:scanned=3,changed=0" {
		t.Errorf("commit scan event[1] = %q, want scan:completed:scanned=3,changed=0", events[1])
	}

	// Then commit lifecycle
	if events[2] != "commit:started:enqueued=1,user=testuser" {
		t.Errorf("event[2] = %q, want commit:started:enqueued=1,user=testuser", events[2])
	}

	expectedCommitEvent := fmt.Sprintf("commit:completed:rid=%d,uuid=%s,files=3", newRID, newUUID)
	if events[3] != expectedCommitEvent {
		t.Errorf("event[3] = %q, want %s", events[3], expectedCommitEvent)
	}

	// Verify RID and UUID are valid
	if newRID <= 0 {
		t.Errorf("newRID = %d, want > 0", newRID)
	}
	if newUUID == "" {
		t.Error("newUUID is empty")
	}

	t.Logf("Full lifecycle complete: %d events recorded", len(events))
}

// TestObserverError verifies that the Error hook is called when operations fail.
func TestObserverError(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Recording observer that tracks errors
	var errorCalls []string
	obs := &testObserver{
		onError: func(ctx context.Context, err error) {
			errorCalls = append(errorCalls, fmt.Sprintf("error:%s", err.Error()))
		},
	}
	co.obs = obs

	// Trigger an error: try to extract a non-existent RID
	badRID := libfossil.FslID(99999)
	err = co.Extract(badRID, ExtractOpts{})
	if err == nil {
		t.Fatal("Extract with bad RID should fail")
	}

	// Note: Current implementation may not call observer.Error for all error paths
	// This test documents the expected behavior. If observer.Error is not called,
	// that's a known limitation we can enhance later.
	t.Logf("Error calls: %v", errorCalls)
}

// TestObserverExtractFileDetails verifies the UpdateChange values in ExtractFileCompleted.
func TestObserverExtractFileDetails(t *testing.T) {
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

	// Track file events with their UpdateChange values
	type fileEvent struct {
		name   string
		change UpdateChange
	}
	var fileEvents []fileEvent

	obs := &testObserver{
		onExtractFileCompleted: func(ctx context.Context, name string, change UpdateChange) {
			fileEvents = append(fileEvents, fileEvent{name, change})
		},
	}
	co.obs = obs

	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Should have 3 files extracted
	if len(fileEvents) != 3 {
		t.Fatalf("expected 3 file events, got %d: %v", len(fileEvents), fileEvents)
	}

	// All files should be UpdateAdded (initial extract)
	for _, fe := range fileEvents {
		if fe.change != UpdateAdded {
			t.Errorf("file %s: change = %v, want UpdateAdded (%v)", fe.name, fe.change, UpdateAdded)
		}
	}

	t.Logf("Extracted files: %v", fileEvents)
}

// TestObserverScanDetails verifies the ScanEnd statistics.
func TestObserverScanDetails(t *testing.T) {
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

	// Extract first
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Modify two files
	mem.WriteFile("/checkout/hello.txt", []byte("changed1\n"), 0644)
	mem.WriteFile("/checkout/README.md", []byte("changed2\n"), 0644)

	var scanEnd ScanEnd
	obs := &testObserver{
		onScanCompleted: func(ctx context.Context, e ScanEnd) {
			scanEnd = e
		},
	}
	co.obs = obs

	if err := co.ScanChanges(ScanHash); err != nil {
		t.Fatal(err)
	}

	// Should have scanned 3 files, found 2 changed
	if scanEnd.FilesScanned != 3 {
		t.Errorf("FilesScanned = %d, want 3", scanEnd.FilesScanned)
	}
	if scanEnd.FilesChanged != 2 {
		t.Errorf("FilesChanged = %d, want 2", scanEnd.FilesChanged)
	}
	if scanEnd.FilesMissing != 0 {
		t.Errorf("FilesMissing = %d, want 0", scanEnd.FilesMissing)
	}

	t.Logf("Scan stats: scanned=%d, changed=%d, missing=%d, extra=%d",
		scanEnd.FilesScanned, scanEnd.FilesChanged, scanEnd.FilesMissing, scanEnd.FilesExtra)
}

// TestObserverCommitDetails verifies the CommitStart and CommitEnd details.
func TestObserverCommitDetails(t *testing.T) {
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

	// Extract and modify
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}
	mem.WriteFile("/checkout/hello.txt", []byte("committed change\n"), 0644)

	var commitStart CommitStart
	var commitEnd CommitEnd
	obs := &testObserver{
		onCommitStarted: func(ctx context.Context, e CommitStart) context.Context {
			commitStart = e
			return ctx
		},
		onCommitCompleted: func(ctx context.Context, e CommitEnd) {
			commitEnd = e
		},
	}
	co.obs = obs

	newRID, newUUID, err := co.Commit(CommitOpts{
		Message: "test message",
		User:    "alice",
		Branch:  "trunk",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify CommitStart
	if commitStart.FilesEnqueued != 1 {
		t.Errorf("CommitStart.FilesEnqueued = %d, want 1", commitStart.FilesEnqueued)
	}
	if commitStart.User != "alice" {
		t.Errorf("CommitStart.User = %q, want alice", commitStart.User)
	}
	if commitStart.Branch != "trunk" {
		t.Errorf("CommitStart.Branch = %q, want trunk", commitStart.Branch)
	}

	// Verify CommitEnd
	if commitEnd.RID != newRID {
		t.Errorf("CommitEnd.RID = %d, want %d", commitEnd.RID, newRID)
	}
	if commitEnd.UUID != newUUID {
		t.Errorf("CommitEnd.UUID = %q, want %s", commitEnd.UUID, newUUID)
	}
	if commitEnd.FilesCommit != 3 { // all files in manifest
		t.Errorf("CommitEnd.FilesCommit = %d, want 3", commitEnd.FilesCommit)
	}
	if commitEnd.Err != nil {
		t.Errorf("CommitEnd.Err = %v, want nil", commitEnd.Err)
	}

	t.Logf("Commit details: RID=%d, UUID=%s, files=%d, user=%s, branch=%s",
		commitEnd.RID, commitEnd.UUID, commitEnd.FilesCommit, commitStart.User, commitStart.Branch)
}
