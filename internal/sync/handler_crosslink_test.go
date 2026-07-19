package sync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/xfer"
)

// buildCheckinManifest constructs a checkin manifest referencing the given
// file blob and returns the wire bytes plus the manifest UUID. Parents may
// be empty for a root-of-tree checkin.
func buildCheckinManifest(t *testing.T, comment string, when time.Time, fileUUID string, fileContent []byte, parents []string) ([]byte, string) {
	t.Helper()
	d := &deck.Deck{
		Type: deck.Checkin,
		C:    comment,
		D:    when,
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
		U:    deck.Str("tester"),
		P:    parents,
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}
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
	return manifestBytes, hash.SHA1(manifestBytes)
}

// TestHandleSync_CrosslinksReceivedManifest verifies that a manifest pushed
// to the server via HandleSync is crosslinked into event/leaf/plink/mlink.
//
// Pre-fix, HandleSync stored the blob durably but did not call the
// crosslink scanner, so the hub's timeline, fork detection, and clone
// protocol all observed empty relational state.
func TestHandleSync_CrosslinksReceivedManifest(t *testing.T) {
	r := setupSyncTestRepo(t)

	fileContent := []byte("hello from crosslink test")
	fileUUID := hash.SHA1(fileContent)

	manifestBytes, manifestUUID := buildCheckinManifest(t,
		"first checkin",
		time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
		fileUUID, fileContent, nil)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: fileUUID, Content: fileContent},
		&xfer.FileCard{UUID: manifestUUID, Content: manifestBytes},
	}}
	if _, err := HandleSync(context.Background(), r, req); err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	manifestRID, ok := blob.Exists(r.DB(), manifestUUID)
	if !ok {
		t.Fatal("manifest blob not stored")
	}

	// event table: one row of type='ci' for the checkin.
	var eventCount int
	if err := r.DB().QueryRow(
		"SELECT count(*) FROM event WHERE type='ci' AND objid=?",
		manifestRID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("count event: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("event ci rows for objid=%d: got %d, want 1", manifestRID, eventCount)
	}

	// leaf table: the checkin is a tip.
	var leafCount int
	if err := r.DB().QueryRow(
		"SELECT count(*) FROM leaf WHERE rid=?", manifestRID,
	).Scan(&leafCount); err != nil {
		t.Fatalf("count leaf: %v", err)
	}
	if leafCount != 1 {
		t.Fatalf("leaf rows for rid=%d: got %d, want 1", manifestRID, leafCount)
	}

	// mlink table: one row mapping the manifest to its file.
	var mlinkCount int
	if err := r.DB().QueryRow(
		"SELECT count(*) FROM mlink WHERE mid=?", manifestRID,
	).Scan(&mlinkCount); err != nil {
		t.Fatalf("count mlink: %v", err)
	}
	if mlinkCount != 1 {
		t.Fatalf("mlink rows for mid=%d: got %d, want 1", manifestRID, mlinkCount)
	}
}

// TestHandleSync_LeafTransitionOnChildCheckin verifies that when a child
// checkin lands via HandleSync, the parent is removed from leaf and the
// child becomes the new tip. This is the operation that produces phantom
// "WouldFork" reports in v0.4.0 when crosslink is missing.
func TestHandleSync_LeafTransitionOnChildCheckin(t *testing.T) {
	r := setupSyncTestRepo(t)

	parentFileContent := []byte("parent file content")
	parentFileUUID := hash.SHA1(parentFileContent)
	parentBytes, parentUUID := buildCheckinManifest(t,
		"parent",
		time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
		parentFileUUID, parentFileContent, nil)

	childFileContent := []byte("child file content")
	childFileUUID := hash.SHA1(childFileContent)
	childBytes, childUUID := buildCheckinManifest(t,
		"child",
		time.Date(2026, 4, 25, 13, 0, 0, 0, time.UTC),
		childFileUUID, childFileContent, []string{parentUUID})

	// Push the parent first.
	if _, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: parentFileUUID, Content: parentFileContent},
		&xfer.FileCard{UUID: parentUUID, Content: parentBytes},
	}}); err != nil {
		t.Fatalf("HandleSync(parent): %v", err)
	}

	parentRID, _ := blob.Exists(r.DB(), parentUUID)

	// Confirm parent is a leaf.
	var leafBefore int
	r.DB().QueryRow("SELECT count(*) FROM leaf WHERE rid=?", parentRID).Scan(&leafBefore)
	if leafBefore != 1 {
		t.Fatalf("after parent push: leaf rows for parent=%d got %d, want 1", parentRID, leafBefore)
	}

	// Push the child.
	if _, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: childFileUUID, Content: childFileContent},
		&xfer.FileCard{UUID: childUUID, Content: childBytes},
	}}); err != nil {
		t.Fatalf("HandleSync(child): %v", err)
	}

	childRID, _ := blob.Exists(r.DB(), childUUID)

	// Parent must be gone from leaf, child must replace it.
	var parentLeaf, childLeaf int
	r.DB().QueryRow("SELECT count(*) FROM leaf WHERE rid=?", parentRID).Scan(&parentLeaf)
	r.DB().QueryRow("SELECT count(*) FROM leaf WHERE rid=?", childRID).Scan(&childLeaf)
	if parentLeaf != 0 {
		t.Errorf("parent leaf count = %d, want 0 (child should have superseded)", parentLeaf)
	}
	if childLeaf != 1 {
		t.Errorf("child leaf count = %d, want 1", childLeaf)
	}

	// plink table: one row connecting parent to child.
	var plinkCount int
	r.DB().QueryRow(
		"SELECT count(*) FROM plink WHERE pid=? AND cid=?",
		parentRID, childRID,
	).Scan(&plinkCount)
	if plinkCount != 1 {
		t.Errorf("plink rows pid=%d cid=%d: got %d, want 1", parentRID, childRID, plinkCount)
	}
}
