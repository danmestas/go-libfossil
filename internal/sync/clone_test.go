package sync_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/delta"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/sync"
	"github.com/danmestas/libfossil/internal/xfer"
)

// mockCloneTransport wraps a handler function for testing.
type mockCloneTransport struct {
	handler func(round int, req *xfer.Message) *xfer.Message
	round   int
}

func (m *mockCloneTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp := m.handler(m.round, req)
	m.round++
	return resp, nil
}

// TestCloneBasic verifies a successful clone with 3 blobs in one round.
func TestCloneBasic(t *testing.T) {
	content1 := []byte("test content 1")
	content2 := []byte("test content 2")
	content3 := []byte("test content 3")
	uuid1 := hash.SHA1(content1)
	uuid2 := hash.SHA1(content2)
	uuid3 := hash.SHA1(content3)

	transport := &mockCloneTransport{
		handler: func(round int, req *xfer.Message) *xfer.Message {
			if round == 0 {
				// First round: push card + 3 file cards + clone_seqno 0
				return &xfer.Message{
					Cards: []xfer.Card{
						&xfer.PushCard{
							ServerCode:  "test-server-code",
							ProjectCode: "test-project-code",
						},
						&xfer.FileCard{UUID: uuid1, Content: content1},
						&xfer.FileCard{UUID: uuid2, Content: content2},
						&xfer.FileCard{UUID: uuid3, Content: content3},
						&xfer.CloneSeqNoCard{SeqNo: 0},
					},
				}
			}
			// Round 1+: empty response with seqno 0
			return &xfer.Message{
				Cards: []xfer.Card{
					&xfer.CloneSeqNoCard{SeqNo: 0},
				},
			}
		},
	}

	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "test.fossil")

	r, result, err := sync.Clone(context.Background(), repoPath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone failed: %v", err)
	}
	defer r.Close()

	// Verify result
	if result.BlobsRecvd != 3 {
		t.Errorf("BlobsRecvd = %d, want 3", result.BlobsRecvd)
	}
	if result.ProjectCode != "test-project-code" {
		t.Errorf("ProjectCode = %q, want %q", result.ProjectCode, "test-project-code")
	}
	if result.Rounds < 2 {
		t.Errorf("Rounds = %d, want >= 2 (min 2 rounds rule)", result.Rounds)
	}

	// Verify blobs exist
	if _, exists := blob.Exists(r.DB(), uuid1); !exists {
		t.Errorf("blob %s not found", uuid1)
	}
	if _, exists := blob.Exists(r.DB(), uuid2); !exists {
		t.Errorf("blob %s not found", uuid2)
	}
	if _, exists := blob.Exists(r.DB(), uuid3); !exists {
		t.Errorf("blob %s not found", uuid3)
	}
}

// TestCloneMultiRound verifies clone with multiple rounds.
func TestCloneMultiRound(t *testing.T) {
	content1 := []byte("test content 1")
	content2 := []byte("test content 2")
	content3 := []byte("test content 3")
	uuid1 := hash.SHA1(content1)
	uuid2 := hash.SHA1(content2)
	uuid3 := hash.SHA1(content3)

	transport := &mockCloneTransport{
		handler: func(round int, req *xfer.Message) *xfer.Message {
			if round == 0 {
				// Round 0: push card + 2 files + seqno 3 (more data coming)
				return &xfer.Message{
					Cards: []xfer.Card{
						&xfer.PushCard{
							ServerCode:  "server-abc",
							ProjectCode: "project-xyz",
						},
						&xfer.FileCard{UUID: uuid1, Content: content1},
						&xfer.FileCard{UUID: uuid2, Content: content2},
						&xfer.CloneSeqNoCard{SeqNo: 3},
					},
				}
			} else if round == 1 {
				// Round 1: final file + seqno 0
				return &xfer.Message{
					Cards: []xfer.Card{
						&xfer.FileCard{UUID: uuid3, Content: content3},
						&xfer.CloneSeqNoCard{SeqNo: 0},
					},
				}
			}
			// Round 2+: no files, seqno 0 (done signal)
			return &xfer.Message{
				Cards: []xfer.Card{
					&xfer.CloneSeqNoCard{SeqNo: 0},
				},
			}
		},
	}

	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "multi.fossil")

	r, result, err := sync.Clone(context.Background(), repoPath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone failed: %v", err)
	}
	defer r.Close()

	// Verify 3 total blobs
	if result.BlobsRecvd != 3 {
		t.Errorf("BlobsRecvd = %d, want 3", result.BlobsRecvd)
	}
	if result.Rounds < 2 {
		t.Errorf("Rounds = %d, want >= 2", result.Rounds)
	}

	// Verify all blobs exist
	if _, exists := blob.Exists(r.DB(), uuid1); !exists {
		t.Errorf("blob %s not found", uuid1)
	}
	if _, exists := blob.Exists(r.DB(), uuid2); !exists {
		t.Errorf("blob %s not found", uuid2)
	}
	if _, exists := blob.Exists(r.DB(), uuid3); !exists {
		t.Errorf("blob %s not found", uuid3)
	}
}

// TestCloneErrorCleansUp verifies that Clone cleans up on server error.
func TestCloneErrorCleansUp(t *testing.T) {
	transport := &mockCloneTransport{
		handler: func(round int, req *xfer.Message) *xfer.Message {
			return &xfer.Message{
				Cards: []xfer.Card{
					&xfer.ErrorCard{Message: "not authorized to clone"},
				},
			}
		},
	}

	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "error.fossil")

	r, _, err := sync.Clone(context.Background(), repoPath, transport, sync.CloneOpts{})
	if err == nil {
		r.Close()
		t.Fatal("Clone should have failed with error card")
	}

	// Verify repo file was deleted
	if _, statErr := os.Stat(repoPath); !os.IsNotExist(statErr) {
		t.Errorf("repo file should be deleted after error, but still exists")
	}
}

// TestCloneExistingPath verifies that Clone fails when path already exists.
func TestCloneExistingPath(t *testing.T) {
	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "existing.fossil")

	// Create a file at the target path
	if err := os.WriteFile(repoPath, []byte("existing file"), 0644); err != nil {
		t.Fatalf("failed to create existing file: %v", err)
	}

	handlerCalled := false
	transport := &mockCloneTransport{
		handler: func(round int, req *xfer.Message) *xfer.Message {
			handlerCalled = true
			return &xfer.Message{
				Cards: []xfer.Card{
					&xfer.PushCard{ProjectCode: "test"},
					&xfer.CloneSeqNoCard{SeqNo: 0},
				},
			}
		},
	}

	r, _, err := sync.Clone(context.Background(), repoPath, transport, sync.CloneOpts{})
	if err == nil {
		r.Close()
		t.Fatal("Clone should fail when path already exists")
	}
	if handlerCalled {
		t.Error("transport handler should not be called when path exists")
	}
}

// TestCloneCancelledContext verifies cleanup when context is cancelled.
func TestCloneCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	transport := &mockCloneTransport{
		handler: func(round int, req *xfer.Message) *xfer.Message {
			if round == 0 {
				return &xfer.Message{
					Cards: []xfer.Card{
						&xfer.PushCard{ProjectCode: "test-project"},
						&xfer.CloneSeqNoCard{SeqNo: 99}, // Pretend more data coming
					},
				}
			}
			// Cancel after round 0 completes
			cancel()
			return &xfer.Message{
				Cards: []xfer.Card{
					&xfer.CloneSeqNoCard{SeqNo: 99},
				},
			}
		},
	}

	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "cancelled.fossil")

	r, _, err := sync.Clone(ctx, repoPath, transport, sync.CloneOpts{})
	if err == nil {
		r.Close()
		t.Fatal("Clone should fail with cancelled context")
	}

	// Verify repo file was deleted
	if _, statErr := os.Stat(repoPath); !os.IsNotExist(statErr) {
		t.Errorf("repo file should be deleted after cancellation, but still exists")
	}
}

// TestCloneWithPhantoms verifies that delta files with missing sources
// create phantoms and are resolved via gimme on subsequent rounds.
func TestCloneWithPhantoms(t *testing.T) {
	// Base blob that the delta depends on.
	baseContent := []byte("this is the base content for delta testing")
	baseUUID := hash.SHA1(baseContent)

	// Target content and its delta against base.
	targetContent := []byte("this is the modified content for delta testing")
	targetUUID := hash.SHA1(targetContent)
	deltaBytes := delta.Create(baseContent, targetContent)

	gimmesSeen := make(map[string]bool)

	transport := &mockCloneTransport{
		handler: func(round int, req *xfer.Message) *xfer.Message {
			// Track gimme cards sent by the client.
			for _, c := range req.Cards {
				if g, ok := c.(*xfer.GimmeCard); ok {
					gimmesSeen[g.UUID] = true
				}
			}

			switch round {
			case 0:
				// Round 0: send delta file BEFORE its base — triggers phantom.
				return &xfer.Message{Cards: []xfer.Card{
					&xfer.PushCard{ServerCode: "s1", ProjectCode: "p1"},
					&xfer.CFileCard{UUID: targetUUID, DeltaSrc: baseUUID, Content: deltaBytes},
					&xfer.CloneSeqNoCard{SeqNo: 0},
				}}
			case 1:
				// Round 1: seqno=0 so client switches to pull+gimme.
				// Deliver the base blob that was requested via gimme.
				return &xfer.Message{Cards: []xfer.Card{
					&xfer.FileCard{UUID: baseUUID, Content: baseContent},
					// Also re-send the delta now that base exists.
					&xfer.CFileCard{UUID: targetUUID, DeltaSrc: baseUUID, Content: deltaBytes},
					&xfer.CloneSeqNoCard{SeqNo: 0},
				}}
			default:
				return &xfer.Message{Cards: []xfer.Card{
					&xfer.CloneSeqNoCard{SeqNo: 0},
				}}
			}
		},
	}

	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "phantom.fossil")

	r, result, err := sync.Clone(context.Background(), repoPath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone failed: %v", err)
	}
	defer r.Close()

	// Verify base blob exists.
	if _, ok := blob.Exists(r.DB(), baseUUID); !ok {
		t.Error("base blob not found after clone")
	}

	// Verify target blob exists (delta resolved).
	if _, ok := blob.Exists(r.DB(), targetUUID); !ok {
		t.Error("target blob not found after clone (delta should be resolved)")
	}

	// Verify gimme was sent for the missing delta source.
	if !gimmesSeen[baseUUID] && !gimmesSeen[targetUUID] {
		t.Error("expected gimme card for phantom UUID, but none was sent")
	}

	t.Logf("Clone with phantoms: rounds=%d blobs=%d", result.Rounds, result.BlobsRecvd)
}

// TestCloneCrosslinksManifests verifies that Clone automatically populates the
// event/plink/leaf tables by crosslinking received manifest blobs.
func TestCloneCrosslinksManifests(t *testing.T) {
	// Build a file blob.
	fileContent := []byte("hello from crosslink test")
	fileUUID := hash.SHA1(fileContent)

	// Build a checkin manifest referencing the file.
	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "test checkin",
		D:    time.Date(2026, 3, 21, 12, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
		U:    "tester",
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}

	// Compute R-card.
	rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
		if uuid == fileUUID {
			return fileContent, nil
		}
		return nil, fmt.Errorf("unknown uuid: %s", uuid)
	})
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	d.R = rHash

	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	manifestUUID := hash.SHA1(manifestBytes)

	transport := &mockCloneTransport{
		handler: func(round int, req *xfer.Message) *xfer.Message {
			if round == 0 {
				return &xfer.Message{Cards: []xfer.Card{
					&xfer.PushCard{ServerCode: "srv1", ProjectCode: "proj1"},
					&xfer.FileCard{UUID: fileUUID, Content: fileContent},
					&xfer.FileCard{UUID: manifestUUID, Content: manifestBytes},
					&xfer.CloneSeqNoCard{SeqNo: 0},
				}}
			}
			return &xfer.Message{Cards: []xfer.Card{
				&xfer.CloneSeqNoCard{SeqNo: 0},
			}}
		},
	}

	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "crosslink.fossil")

	r, result, err := sync.Clone(context.Background(), repoPath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone failed: %v", err)
	}
	defer r.Close()

	// Verify blobs received.
	if result.BlobsRecvd != 2 {
		t.Errorf("BlobsRecvd = %d, want 2", result.BlobsRecvd)
	}

	// Verify crosslink populated the event table.
	if result.ArtifactsLinked != 1 {
		t.Errorf("ArtifactsLinked = %d, want 1", result.ArtifactsLinked)
	}

	var eventCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&eventCount); err != nil {
		t.Fatalf("query event: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("event count = %d, want 1", eventCount)
	}

	// Verify leaf table has the manifest.
	var leafCount int
	if err := r.DB().QueryRow("SELECT count(*) FROM leaf").Scan(&leafCount); err != nil {
		t.Fatalf("query leaf: %v", err)
	}
	if leafCount != 1 {
		t.Errorf("leaf count = %d, want 1", leafCount)
	}

	// Verify manifest blob exists.
	if _, ok := blob.Exists(r.DB(), manifestUUID); !ok {
		t.Error("manifest blob not found")
	}

	t.Logf("Clone crosslink: rounds=%d blobs=%d checkins=%d", result.Rounds, result.BlobsRecvd, result.ArtifactsLinked)
}

// TestCloneViaHandler wires Clone() against HandleSync() with a real repo.
// This is the integration test that catches protocol mismatches between
// client and server — missing PushCard, missing completion signal, etc.
func TestCloneViaHandler(t *testing.T) {
	dir := t.TempDir()

	// Create source repo with a real checkin.
	srcPath := filepath.Join(dir, "source.fossil")
	srcRepo, err := repo.Create(srcPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srcRepo.Close()

	_, _, err = manifest.Checkin(srcRepo, manifest.CheckinOpts{
		Comment: "initial commit",
		User:    "testuser",
		Files: []manifest.File{
			{Name: "README.md", Content: []byte("# Test repo\n")},
			{Name: "hello.txt", Content: []byte("hello world\n")},
			{Name: "src/main.go", Content: []byte("package main\n")},
		},
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Read source project-code for verification.
	var srcProjectCode string
	srcRepo.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&srcProjectCode)

	// Wire Clone() against HandleSync() via MockTransport.
	transport := &sync.MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, err := sync.HandleSync(context.Background(), srcRepo, req)
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			return resp
		},
	}

	clonePath := filepath.Join(dir, "clone.fossil")
	cloneRepo, result, err := sync.Clone(context.Background(), clonePath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer cloneRepo.Close()

	// Verify blobs received (3 file blobs + 1 manifest).
	if result.BlobsRecvd < 4 {
		t.Errorf("BlobsRecvd = %d, want >= 4", result.BlobsRecvd)
	}

	// Verify crosslink produced a checkin.
	if result.ArtifactsLinked != 1 {
		t.Errorf("ArtifactsLinked = %d, want 1", result.ArtifactsLinked)
	}

	// Verify project-code propagated.
	if result.ProjectCode != srcProjectCode {
		t.Errorf("ProjectCode = %q, want %q", result.ProjectCode, srcProjectCode)
	}

	// Verify tipRID query works (leaf + event tables populated).
	var tipRID int64
	err = cloneRepo.DB().QueryRow(`
		SELECT l.rid FROM leaf l
		JOIN event e ON e.objid=l.rid
		WHERE e.type='ci'
		ORDER BY e.mtime DESC LIMIT 1
	`).Scan(&tipRID)
	if err != nil {
		t.Fatalf("tipRID query: %v", err)
	}
	if tipRID <= 0 {
		t.Errorf("tipRID = %d, want > 0", tipRID)
	}
}

// TestCloneViaHandlerMultipleCheckins verifies clone with parent-child checkins.
func TestCloneViaHandlerMultipleCheckins(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "source.fossil")
	srcRepo, err := repo.Create(srcPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srcRepo.Close()

	// First checkin.
	rid1, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
		Comment: "first",
		User:    "testuser",
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("v1")}},
	})
	if err != nil {
		t.Fatalf("Checkin 1: %v", err)
	}

	// Second checkin (child of first).
	rid2, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
		Comment: "second",
		User:    "testuser",
		Parent:  rid1,
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("v2")}, {Name: "b.txt", Content: []byte("new")}},
	})
	if err != nil {
		t.Fatalf("Checkin 2: %v", err)
	}

	// Third checkin (child of second).
	_, _, err = manifest.Checkin(srcRepo, manifest.CheckinOpts{
		Comment: "third",
		User:    "testuser",
		Parent:  rid2,
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("v3")}, {Name: "b.txt", Content: []byte("new")}},
	})
	if err != nil {
		t.Fatalf("Checkin 3: %v", err)
	}

	transport := &sync.MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, err := sync.HandleSync(context.Background(), srcRepo, req)
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			return resp
		},
	}

	clonePath := filepath.Join(dir, "clone.fossil")
	cloneRepo, result, err := sync.Clone(context.Background(), clonePath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer cloneRepo.Close()

	if result.ArtifactsLinked != 3 {
		t.Errorf("ArtifactsLinked = %d, want 3", result.ArtifactsLinked)
	}

	// Verify plink has 2 parent-child links.
	var plinkCount int
	cloneRepo.DB().QueryRow("SELECT count(*) FROM plink").Scan(&plinkCount)
	if plinkCount != 2 {
		t.Errorf("plink count = %d, want 2", plinkCount)
	}

	// Verify 3 events.
	var eventCount int
	cloneRepo.DB().QueryRow("SELECT count(*) FROM event WHERE type='ci'").Scan(&eventCount)
	if eventCount != 3 {
		t.Errorf("event count = %d, want 3", eventCount)
	}
}

// siteAlways is a BuggifyChecker that fires only for the named site.
// External-package counterpart to internal alwaysAtSite in handler_test.go.
type siteAlways string

func (s siteAlways) Check(site string, _ float64) bool { return string(s) == site }

// TestCloneAgainstWritingHub reproduces issue #17.
//
// When cloning a hub that is being written to during the clone session
// (e.g. autosyncing leaves committing while a new leaf clones the hub),
// the server's emitCloneBatch issues `WHERE rid > cursor` with no upper
// bound, so newly-arrived blobs keep extending the queue. The completion
// signal CloneSeqNoCard{SeqNo:0} never fires while the hub keeps growing,
// the client's `done && seqno <= 0` gate stays false, and the round loop
// runs to MaxRounds.
//
// We squeeze the per-round batch via the smallBatch Buggify hook so the
// bug fires deterministically with a single new blob per round; the
// production trigger is the same shape with a 200-blob default batch.
//
// After the fix, the server snapshots max(rid) at the start of the clone
// session and bounds subsequent batches by it, so the clone converges.
func TestCloneAgainstWritingHub(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "source.fossil")
	srcRepo, err := repo.Create(srcPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srcRepo.Close()

	if _, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
		Comment: "initial",
		User:    "testuser",
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("hello")}},
	}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	bug := siteAlways("handler.emitCloneBatch.smallBatch")

	roundCounter := 0
	transport := &sync.MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			// Simulate a hub being written to between clone rounds.
			payload := []byte(fmt.Sprintf("growth-blob-%d", roundCounter))
			if _, _, err := blob.Store(srcRepo.DB(), payload); err != nil {
				t.Fatalf("blob.Store: %v", err)
			}
			roundCounter++
			resp, err := sync.HandleSyncWithOpts(context.Background(), srcRepo, req, sync.HandleOpts{Buggify: bug})
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			return resp
		},
	}

	clonePath := filepath.Join(dir, "clone.fossil")
	cloneRepo, result, err := sync.Clone(context.Background(), clonePath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone failed (issue #17 reproduced): %v", err)
	}
	defer cloneRepo.Close()

	if result.Rounds >= sync.MaxRounds {
		t.Errorf("Rounds = %d, hit MaxRounds (%d) — clone did not converge against writing hub", result.Rounds, sync.MaxRounds)
	}

	// Sanity: clone received at least one blob (the seeded checkin's content).
	if result.BlobsRecvd < 1 {
		t.Errorf("BlobsRecvd = %d, want >= 1", result.BlobsRecvd)
	}
}

// TestCloneMultiRoundCursor independently exercises the rid-cursor advance
// across multiple clone rounds. Pre-fix the libfossil clone client only
// emitted CloneCard (which signals clone mode) and never CloneSeqNoCard
// (which carries the actual pagination cursor server-side, per the
// pagination protocol exercised by TestHandleClonePagination). The bug was
// masked by every existing clone-client test fitting in one batch
// (DefaultCloneBatchSize=200); smallBatch=1 forces multi-round and would
// otherwise loop on the same rid prefix until MaxRounds.
func TestCloneMultiRoundCursor(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "source.fossil")
	srcRepo, err := repo.Create(srcPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srcRepo.Close()

	// Three checkins → enough blobs to require several rounds at batchSize=1.
	for i := 0; i < 3; i++ {
		if _, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
			Comment: fmt.Sprintf("checkin %d", i),
			User:    "testuser",
			Files:   []manifest.File{{Name: "a.txt", Content: []byte(fmt.Sprintf("v%d", i))}},
		}); err != nil {
			t.Fatalf("Checkin %d: %v", i, err)
		}
	}

	var srcBlobCount int
	if err := srcRepo.DB().QueryRow("SELECT COUNT(*) FROM blob WHERE size >= 0").Scan(&srcBlobCount); err != nil {
		t.Fatalf("count source blobs: %v", err)
	}

	bug := siteAlways("handler.emitCloneBatch.smallBatch")

	transport := &sync.MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, err := sync.HandleSyncWithOpts(context.Background(), srcRepo, req, sync.HandleOpts{Buggify: bug})
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			return resp
		},
	}

	clonePath := filepath.Join(dir, "clone.fossil")
	cloneRepo, result, err := sync.Clone(context.Background(), clonePath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer cloneRepo.Close()

	// All distinct blobs delivered, not the same prefix on repeat.
	if result.BlobsRecvd < srcBlobCount {
		t.Errorf("BlobsRecvd = %d, want >= %d (source blob count)", result.BlobsRecvd, srcBlobCount)
	}

	var clonedBlobCount int
	if err := cloneRepo.DB().QueryRow("SELECT COUNT(*) FROM blob WHERE size >= 0").Scan(&clonedBlobCount); err != nil {
		t.Fatalf("count cloned blobs: %v", err)
	}
	if clonedBlobCount != srcBlobCount {
		t.Errorf("cloned distinct blobs = %d, want %d", clonedBlobCount, srcBlobCount)
	}
}

// TestCloneSnapshotBoundExcludesPostSnapshotWrites verifies the server's
// snapshot bound semantic: blobs written to the source repo *after* the
// clone session opens are not included in the clone result. Without this,
// a clone against a writing hub never reaches the seqno=0 completion
// signal (issue #17).
func TestCloneSnapshotBoundExcludesPostSnapshotWrites(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "source.fossil")
	srcRepo, err := repo.Create(srcPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srcRepo.Close()

	if _, _, err := manifest.Checkin(srcRepo, manifest.CheckinOpts{
		Comment: "initial",
		User:    "testuser",
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("hello")}},
	}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	var snapshotBlobCount int
	if err := srcRepo.DB().QueryRow("SELECT COUNT(*) FROM blob WHERE size >= 0").Scan(&snapshotBlobCount); err != nil {
		t.Fatalf("snapshot count: %v", err)
	}

	postSnapPayload := []byte("written-after-clone-starts-marker")

	bug := siteAlways("handler.emitCloneBatch.smallBatch")

	roundCounter := 0
	transport := &sync.MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			// Inject a marker write on round 1 (after the snapshot was taken
			// on round 0). The clone must not pick this up.
			if roundCounter == 1 {
				if _, _, err := blob.Store(srcRepo.DB(), postSnapPayload); err != nil {
					t.Fatalf("blob.Store: %v", err)
				}
			}
			roundCounter++
			resp, err := sync.HandleSyncWithOpts(context.Background(), srcRepo, req, sync.HandleOpts{Buggify: bug})
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			return resp
		},
	}

	clonePath := filepath.Join(dir, "clone.fossil")
	cloneRepo, _, err := sync.Clone(context.Background(), clonePath, transport, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer cloneRepo.Close()

	postSnapUUID := hash.SHA1(postSnapPayload)
	var found int
	if err := cloneRepo.DB().QueryRow("SELECT COUNT(*) FROM blob WHERE uuid = ?", postSnapUUID).Scan(&found); err != nil {
		t.Fatalf("query post-snap uuid: %v", err)
	}
	if found != 0 {
		t.Errorf("clone received post-snapshot blob (uuid %s); snapshot bound not enforced", postSnapUUID)
	}
}
