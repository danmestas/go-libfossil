package sync

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/libfossil/internal/auth"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/internal/xfer"
)

// findCards returns all cards of type T from a message.
func findCards[T xfer.Card](msg *xfer.Message) []T {
	var out []T
	for _, c := range msg.Cards {
		if tc, ok := c.(T); ok {
			out = append(out, tc)
		}
	}
	return out
}

// storeTestBlob stores a blob and returns its UUID.
func storeTestBlob(t *testing.T, r *repo.Repo, data []byte) string {
	t.Helper()
	uuid := hash.SHA1(data)
	if err := storeReceivedFile(r, uuid, "", data, nil); err != nil {
		t.Fatalf("storeReceivedFile: %v", err)
	}
	return uuid
}

func testProjectCode(t *testing.T, d *db.DB) string {
	t.Helper()
	var code string
	if err := d.QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&code); err != nil {
		t.Fatalf("project-code: %v", err)
	}
	return code
}

func buildTestLoginCard(user, password, projectCode string, payload []byte) *xfer.LoginCard {
	nonce := testSHA1Hex(payload)
	shared := testSHA1Hex([]byte(projectCode + "/" + user + "/" + password))
	sig := testSHA1Hex([]byte(nonce + shared))
	return &xfer.LoginCard{User: user, Nonce: nonce, Signature: sig}
}

func testSHA1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}


func TestHandlePull(t *testing.T) {
	r := setupSyncTestRepo(t)
	uuid := storeTestBlob(t, r, []byte("pull me"))

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	igots := findCards[*xfer.IGotCard](resp)
	found := false
	for _, ig := range igots {
		if ig.UUID == uuid {
			found = true
		}
	}
	if !found {
		t.Fatalf("pull response missing igot for %s", uuid)
	}
}

func TestHandleIGotGimme(t *testing.T) {
	r := setupSyncTestRepo(t)
	unknownUUID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.IGotCard{UUID: unknownUUID},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	gimmes := findCards[*xfer.GimmeCard](resp)
	found := false
	for _, g := range gimmes {
		if g.UUID == unknownUUID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected gimme for unknown UUID")
	}
}

// TestHandleIGotWithoutPushOrPull verifies the server does not gimme when
// the client is neither pushing nor pulling. Without pushOK there is no
// mandate for the client to send files, so issuing a gimme is wasted.
func TestHandleIGotWithoutPushOrPull(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.IGotCard{UUID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	gimmes := findCards[*xfer.GimmeCard](resp)
	if len(gimmes) > 0 {
		t.Fatal("should not gimme without push or pull card")
	}
}

// TestHandleIGotWithPushOnly verifies the server emits gimme cards when
// the client is push-only. This is the multi-round push-without-pull case
// — without it, push-only clients exit after one round even when the
// server is missing artifacts the client has just announced.
func TestHandleIGotWithPushOnly(t *testing.T) {
	r := setupSyncTestRepo(t)
	unknownUUID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.IGotCard{UUID: unknownUUID},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	gimmes := findCards[*xfer.GimmeCard](resp)
	found := false
	for _, g := range gimmes {
		if g.UUID == unknownUUID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected gimme for unknown UUID under push-only")
	}
}

func TestHandleGimme(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("gimme this")
	uuid := storeTestBlob(t, r, data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.GimmeCard{UUID: uuid},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	files := findCards[*xfer.FileCard](resp)
	found := false
	for _, f := range files {
		if f.UUID == uuid && string(f.Content) == string(data) {
			found = true
		}
	}
	if !found {
		t.Fatal("expected file card with correct content")
	}
}

func TestHandleGimmeMissing(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.GimmeCard{UUID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	files := findCards[*xfer.FileCard](resp)
	if len(files) > 0 {
		t.Fatal("should not return file for missing blob")
	}
}

func TestHandlePushFile(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("push this")
	uuid := hash.SHA1(data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}

	_, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatal("pushed blob not stored")
	}
}

func TestHandleFileWithoutPush(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("no push card")
	uuid := hash.SHA1(data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) == 0 {
		t.Fatal("expected error for file without push")
	}
}

func TestHandleClone(t *testing.T) {
	r := setupSyncTestRepo(t)
	stored := map[string]bool{}
	for i := range 5 {
		data := []byte(fmt.Sprintf("clone test %d", i))
		uuid := storeTestBlob(t, r, data)
		stored[uuid] = true
	}

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	files := findCards[*xfer.FileCard](resp)
	for _, f := range files {
		delete(stored, f.UUID)
	}
	if len(stored) > 0 {
		t.Fatalf("clone missing blobs: %v", stored)
	}
}

func TestHandleClonePagination(t *testing.T) {
	r := setupSyncTestRepo(t)
	for i := range DefaultCloneBatchSize + 5 {
		data := []byte(fmt.Sprintf("page blob %d", i))
		storeTestBlob(t, r, data)
	}

	// Page 1
	req1 := &xfer.Message{Cards: []xfer.Card{&xfer.CloneCard{Version: 1}}}
	resp1, err := HandleSync(context.Background(), r, req1)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}

	files1 := findCards[*xfer.FileCard](resp1)
	seqnos := findCards[*xfer.CloneSeqNoCard](resp1)
	if len(seqnos) == 0 {
		t.Fatal("page 1: expected clone_seqno card for continuation")
	}
	if len(files1) != DefaultCloneBatchSize {
		t.Fatalf("page 1: got %d files, want %d", len(files1), DefaultCloneBatchSize)
	}

	// Page 2
	req2 := &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1},
		&xfer.CloneSeqNoCard{SeqNo: seqnos[0].SeqNo},
	}}
	resp2, err := HandleSync(context.Background(), r, req2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}

	files2 := findCards[*xfer.FileCard](resp2)
	if len(files2) < 5 {
		t.Fatalf("page 2: got %d files, want >= 5", len(files2))
	}

	seqnos2 := findCards[*xfer.CloneSeqNoCard](resp2)
	if len(seqnos2) != 1 || seqnos2[0].SeqNo != 0 {
		t.Fatalf("page 2: expected clone_seqno 0 (completion), got %v", seqnos2)
	}
}

func TestHandleReqConfig(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.ReqConfigCard{Name: "project-code"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	configs := findCards[*xfer.ConfigCard](resp)
	found := false
	for _, c := range configs {
		if c.Name == "project-code" && len(c.Content) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected config card for project-code")
	}
}

func TestHandlePushFileStoreFails(t *testing.T) {
	r := setupSyncTestRepo(t)
	// File with valid push but bad hash → storeReceivedFile returns error → ErrorCard
	badUUID := "cccccccccccccccccccccccccccccccccccccccc"
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: badUUID, Content: []byte("wrong hash content")},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) == 0 {
		t.Fatal("expected error card for bad hash")
	}
}

// alwaysAtSite is a BuggifyChecker that fires only for one named site.
type alwaysAtSite string

func (s alwaysAtSite) Check(site string, _ float64) bool { return string(s) == site }

// TestHandleFileMissingAfterStore exercises the regression path from
// issue #14: blob.Exists returning ok=false after a successful
// storeReceivedFile must not reach content.MakePublic / MakePrivate
// (which panic on rid <= 0). The handler should emit an ErrorCard
// and continue processing.
func TestHandleFileMissingAfterStore(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("payload that stores cleanly")
	uuid := hash.SHA1(data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	opts := HandleOpts{Buggify: alwaysAtSite("handler.handleFile.missingAfterStore")}
	resp, err := HandleSyncWithOpts(context.Background(), r, req, opts)
	if err != nil {
		t.Fatalf("HandleSyncWithOpts: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) != 1 {
		t.Fatalf("error cards = %d, want 1", len(errs))
	}
	if want := "missing after store"; !strings.Contains(errs[0].Message, want) {
		t.Fatalf("error card message = %q, want containing %q", errs[0].Message, want)
	}
}

func TestHandleCFileCard(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("cfile content")
	uuid := hash.SHA1(data)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.CFileCard{UUID: uuid, Content: data, USize: len(data)},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}
	_, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatal("cfile blob not stored")
	}
}

func TestHandleLoginAndPragma(t *testing.T) {
	r := setupSyncTestRepo(t)
	storeTestBlob(t, r, []byte("login pragma test"))

	// Pragma cards should be accepted (no login needed for pragma processing)
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "client-version", Values: []string{"22800"}},
		&xfer.PragmaCard{Name: "unknown-pragma", Values: []string{"ignored"}},
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}
	igots := findCards[*xfer.IGotCard](resp)
	if len(igots) == 0 {
		t.Fatal("expected igot cards after pragma+pull")
	}
}

func TestHandleCloneSeqNo(t *testing.T) {
	r := setupSyncTestRepo(t)
	// Store blobs, then clone with a seqno that skips them
	for i := range 3 {
		data := []byte(fmt.Sprintf("seqno test %d", i))
		storeTestBlob(t, r, data)
	}

	// Get all blobs to find the max rid
	req1 := &xfer.Message{Cards: []xfer.Card{&xfer.CloneCard{Version: 1}}}
	resp1, _ := HandleSync(context.Background(), r, req1)
	files1 := findCards[*xfer.FileCard](resp1)

	// Now clone with seqno past all blobs — should get nothing
	req2 := &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1},
		&xfer.CloneSeqNoCard{SeqNo: 9999},
	}}
	resp2, err := HandleSync(context.Background(), r, req2)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	files2 := findCards[*xfer.FileCard](resp2)
	if len(files2) > 0 {
		t.Fatalf("expected no files with high seqno, got %d", len(files2))
	}

	_ = files1 // used for context
}

func TestHandleEmptyRequest(t *testing.T) {
	r := setupSyncTestRepo(t)
	req := &xfer.Message{Cards: nil}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	if len(resp.Cards) != 0 {
		t.Fatalf("expected empty response for empty request, got %d cards", len(resp.Cards))
	}
}

func TestHandleReqConfigMissing(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.ReqConfigCard{Name: "nonexistent-config"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	configs := findCards[*xfer.ConfigCard](resp)
	if len(configs) > 0 {
		t.Fatal("should not return config for nonexistent key")
	}
}

// TestHandleSyncNoSpuriousGimmeForReceivedFile verifies that HandleSync
// does NOT emit a GimmeCard for a blob that was delivered as a FileCard
// in the same request. Regression test for the igot-before-file bug:
// if IGotCard is processed before FileCard, blob.Exists returns false
// and a spurious GimmeCard is emitted, causing infinite sync loops.
func TestHandleSyncNoSpuriousGimmeForReceivedFile(t *testing.T) {
	r := setupSyncTestRepo(t)
	defer r.Close()

	// Create a blob to push to the handler.
	content := []byte("test content for spurious gimme check")
	uuid := hash.SHA1(content)

	// Build a request with BOTH IGotCard and FileCard for the same blob.
	// This mimics what a sync client sends when pushing a new blob:
	// it announces via igot AND delivers the file in the same round.
	req := &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PushCard{
				ServerCode:  "test-server",
				ProjectCode: "test-project",
			},
			&xfer.PullCard{
				ServerCode:  "test-server",
				ProjectCode: "test-project",
			},
			&xfer.IGotCard{UUID: uuid},
			&xfer.FileCard{UUID: uuid, Content: content},
		},
	}

	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// Check response for spurious gimme.
	for _, card := range resp.Cards {
		if g, ok := card.(*xfer.GimmeCard); ok && g.UUID == uuid {
			t.Errorf("HandleSync emitted GimmeCard for %s which was delivered as FileCard in the same request — this causes infinite sync loops", uuid[:12])
		}
	}

	// Verify the blob was actually stored.
	if _, exists := blob.Exists(r.DB(), uuid); !exists {
		t.Errorf("blob %s was not stored by HandleSync", uuid[:12])
	}

	// Server should NOT emit igot for this blob because the client
	// announced it via igot — remoteHas filtering suppresses it.
	for _, card := range resp.Cards {
		if ig, ok := card.(*xfer.IGotCard); ok && ig.UUID == uuid {
			t.Errorf("HandleSync should not emit IGotCard for %s — client already announced it via igot", uuid[:12])
		}
	}
}

func TestHandlerEmitsPushCardOnClone(t *testing.T) {
	r := setupSyncTestRepo(t)

	var projectCode, serverCode string
	r.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projectCode)
	r.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&serverCode)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	pushCards := findCards[*xfer.PushCard](resp)
	if len(pushCards) != 1 {
		t.Fatalf("PushCard count = %d, want 1", len(pushCards))
	}
	if pushCards[0].ProjectCode != projectCode {
		t.Errorf("ProjectCode = %q, want %q", pushCards[0].ProjectCode, projectCode)
	}
	if pushCards[0].ServerCode != serverCode {
		t.Errorf("ServerCode = %q, want %q", pushCards[0].ServerCode, serverCode)
	}
}

func TestHandlerNoPushCardOnPull(t *testing.T) {
	r := setupSyncTestRepo(t)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// Server must NOT emit PushCard on sync/pull — real Fossil treats
	// server-sent "push" as unknown command during sync.
	pushCards := findCards[*xfer.PushCard](resp)
	if len(pushCards) != 0 {
		t.Fatalf("PushCard count = %d, want 0 (push is clone-only)", len(pushCards))
	}
}

// TestEmitIGots_OnlyUnclustered verifies that after clustering, emitIGots
// returns only unclustered entries (not all blobs in the repo).
func TestEmitIGots_AllBlobs(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 200 blobs — above ClusterThreshold (100).
	for i := 0; i < 200; i++ {
		data := []byte(fmt.Sprintf("blob-%04d", i))
		if _, _, err := blob.Store(r.DB(), data); err != nil {
			t.Fatalf("Store blob %d: %v", i, err)
		}
	}

	// Pre-cluster so we have known state before the handler runs.
	n, err := content.GenerateClusters(r.DB())
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least 1 cluster to be created")
	}

	// Send a pull request — handler emits igots for ALL non-phantom blobs.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
	)

	igots := cardsByType(resp, xfer.CardIGot)

	var totalBlobs int
	r.DB().QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&totalBlobs)

	if len(igots) != totalBlobs {
		t.Fatalf("igots = %d, total blobs = %d; emitIGots should send all blobs",
			len(igots), totalBlobs)
	}
}

// TestPragmaReqClusters verifies that pragma req-clusters causes the handler
// to emit igot cards for cluster artifacts via sendAllClusters.
func TestPragmaReqClusters(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 200 blobs — above ClusterThreshold (100).
	for i := 0; i < 200; i++ {
		data := []byte(fmt.Sprintf("cluster-blob-%04d", i))
		if _, _, err := blob.Store(r.DB(), data); err != nil {
			t.Fatalf("Store blob %d: %v", i, err)
		}
	}

	// Pre-cluster so we have known state.
	n, err := content.GenerateClusters(r.DB())
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 1 {
		t.Fatalf("clusters = %d, want 1", n)
	}

	resp, err := HandleSync(context.Background(), r, &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
			&xfer.PragmaCard{Name: "req-clusters"},
		},
	})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	igots := cardsByType(resp, xfer.CardIGot)

	// emitIGots sends all blobs; sendAllClusters may add cluster igots
	// (deduplication happens client-side). Total should include all blobs.
	var totalBlobs int
	r.DB().QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&totalBlobs)

	if len(igots) < totalBlobs {
		t.Fatalf("igots = %d, total blobs = %d; should include at least all blobs",
			len(igots), totalBlobs)
	}
}

// TestPragmaReqClusters_OldClusters verifies that all blobs are advertised
// even when the unclustered table is empty (all blobs have been clustered).
func TestPragmaReqClusters_OldClusters(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 200 blobs and cluster them.
	for i := 0; i < 200; i++ {
		data := []byte(fmt.Sprintf("old-cluster-blob-%04d", i))
		if _, _, err := blob.Store(r.DB(), data); err != nil {
			t.Fatalf("Store blob %d: %v", i, err)
		}
	}

	n, err := content.GenerateClusters(r.DB())
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n != 1 {
		t.Fatalf("first pass clusters = %d, want 1", n)
	}

	// Manually remove everything from unclustered to simulate old clusters
	// that have been clustered in a future pass.
	if _, err := r.DB().Exec("DELETE FROM unclustered"); err != nil {
		t.Fatalf("clearing unclustered: %v", err)
	}

	resp, err := HandleSync(context.Background(), r, &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
			&xfer.PragmaCard{Name: "req-clusters"},
		},
	})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	igots := cardsByType(resp, xfer.CardIGot)

	// emitIGots sends ALL blobs regardless of unclustered status.
	// sendAllClusters adds cluster igots (may duplicate).
	var totalBlobs int
	r.DB().QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&totalBlobs)

	if len(igots) < totalBlobs {
		t.Fatalf("igots = %d, total blobs = %d; should include all blobs",
			len(igots), totalBlobs)
	}
}

func TestHandlePushRequiresAuth(t *testing.T) {
	r := setupSyncTestRepo(t)
	// Delete nobody so anonymous push is rejected
	r.DB().Exec("DELETE FROM user WHERE login='nobody'")

	data := []byte("auth test")
	uuid := hash.SHA1(data)
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) == 0 {
		t.Fatal("expected error for unauthorized push")
	}
	if _, ok := blob.Exists(r.DB(), uuid); ok {
		t.Fatal("blob should not be stored without push capability")
	}
}

func TestHandlePullRequiresAuth(t *testing.T) {
	r := setupSyncTestRepo(t)
	storeTestBlob(t, r, []byte("pull auth test"))
	// Delete nobody so anonymous pull is rejected
	r.DB().Exec("DELETE FROM user WHERE login='nobody'")

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) == 0 {
		t.Fatal("expected error for unauthorized pull")
	}
	igots := findCards[*xfer.IGotCard](resp)
	if len(igots) > 0 {
		t.Fatal("should not emit igots without pull capability")
	}
}

func TestHandleAuthenticatedPush(t *testing.T) {
	r := setupSyncTestRepo(t)
	pc := testProjectCode(t, r.DB())
	// Delete nobody, create a user with push caps
	r.DB().Exec("DELETE FROM user WHERE login='nobody'")
	auth.CreateUser(r.DB(), pc, "pusher", "secret", "oi")

	data := []byte("authed push")
	uuid := hash.SHA1(data)

	// Build a valid login card — nonce is SHA1 of the non-login card payload
	loginCard := buildTestLoginCard("pusher", "secret", pc, []byte("dummy"))

	req := &xfer.Message{Cards: []xfer.Card{
		loginCard,
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](resp)
	if len(errs) > 0 {
		t.Fatalf("unexpected error: %s", errs[0].Message)
	}
	if _, ok := blob.Exists(r.DB(), uuid); !ok {
		t.Fatal("authenticated push should store blob")
	}
}

func TestHandleNobodyPullOnly(t *testing.T) {
	r := setupSyncTestRepo(t)
	storeTestBlob(t, r, []byte("nobody test"))
	// Set nobody to pull-only
	r.DB().Exec("UPDATE user SET cap='o' WHERE login='nobody'")

	// Pull should work
	pullReq := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	pullResp, _ := HandleSync(context.Background(), r, pullReq)
	igots := findCards[*xfer.IGotCard](pullResp)
	if len(igots) == 0 {
		t.Fatal("nobody with 'o' cap should allow pull")
	}

	// Push should fail
	data := []byte("nobody push attempt")
	uuid := hash.SHA1(data)
	pushReq := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	pushResp, _ := HandleSync(context.Background(), r, pushReq)
	errs := findCards[*xfer.ErrorCard](pushResp)
	if len(errs) == 0 {
		t.Fatal("nobody with 'o' cap should reject push")
	}
}

func TestEmitIGots_ExcludesShunAndPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)

	_, normalUUID, err := blob.Store(r.DB(), []byte("normal-blob-content"))
	if err != nil {
		t.Fatalf("Store normal: %v", err)
	}
	_, shunnedUUID, err := blob.Store(r.DB(), []byte("shunned-blob-content"))
	if err != nil {
		t.Fatalf("Store shunned: %v", err)
	}
	privRid, _, err := blob.Store(r.DB(), []byte("private-blob-content"))
	if err != nil {
		t.Fatalf("Store private: %v", err)
	}

	if _, err := r.DB().Exec("INSERT INTO shun(uuid, mtime) VALUES(?, 0)", shunnedUUID); err != nil {
		t.Fatalf("shun: %v", err)
	}
	if _, err := r.DB().Exec("INSERT INTO private(rid) VALUES(?)", privRid); err != nil {
		t.Fatalf("private: %v", err)
	}

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	igotUUIDs := make(map[string]bool)
	for _, c := range resp.Cards {
		if ig, ok := c.(*xfer.IGotCard); ok {
			igotUUIDs[ig.UUID] = true
		}
	}

	if !igotUUIDs[normalUUID] {
		t.Error("normal blob missing from igots")
	}

	if igotUUIDs[shunnedUUID] {
		t.Error("shunned blob appeared in igots")
	}

	var privUUID string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", privRid).Scan(&privUUID); err != nil {
		t.Fatalf("query privUUID: %v", err)
	}
	if igotUUIDs[privUUID] {
		t.Error("private blob appeared in igots")
	}
}

func TestHandlerPragmaSendPrivate_Accepted(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
	)
	for _, c := range resp.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			if e.Message == "not authorized to sync private content" {
				t.Error("should not get auth error with 'x' capability")
			}
		}
	}
}

func TestHandlerPragmaSendPrivate_Rejected(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oi' WHERE login='nobody'")
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
	)
	errors := findCards[*xfer.ErrorCard](resp)
	found := false
	for _, e := range errors {
		if e.Message == "not authorized to sync private content" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'not authorized to sync private content' error")
	}
}

func TestHandlerPrivateCardAccepted(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")
	data := []byte("private blob data")
	uuid := hash.SHA1(data)

	resp := handleReq(t, r,
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PrivateCard{},
		&xfer.FileCard{UUID: uuid, Content: data},
	)

	for _, c := range resp.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			t.Errorf("unexpected error: %s", e.Message)
		}
	}

	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatal("blob not stored")
	}
	if !content.IsPrivate(r.DB(), int64(rid)) {
		t.Error("blob should be marked private")
	}
}

func TestHandlerPrivateCardRejected(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oi' WHERE login='nobody'")
	data := []byte("private blob rejected")
	uuid := hash.SHA1(data)

	resp := handleReq(t, r,
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PrivateCard{},
		&xfer.FileCard{UUID: uuid, Content: data},
	)

	errors := findCards[*xfer.ErrorCard](resp)
	found := false
	for _, e := range errors {
		if e.Message == "not authorized to sync private content" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'not authorized to sync private content' error")
	}
}

func TestHandlerPublicFileClearsPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("was private now public")
	uuid := hash.SHA1(data)

	// Pre-store as private.
	storeReceivedFile(r, uuid, "", data, nil)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))
	if !content.IsPrivate(r.DB(), int64(rid)) {
		t.Fatal("precondition: blob should be private")
	}

	// Push same blob WITHOUT private card — should clear private.
	resp := handleReq(t, r,
		&xfer.PushCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.FileCard{UUID: uuid, Content: data},
	)

	for _, c := range resp.Cards {
		if e, ok := c.(*xfer.ErrorCard); ok {
			t.Errorf("unexpected error: %s", e.Message)
		}
	}

	if content.IsPrivate(r.DB(), int64(rid)) {
		t.Error("blob should no longer be private after public file push")
	}
}

func TestEmitIGotsExcludesPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)
	pubUUID := storeTestBlob(t, r, []byte("public blob"))
	privData := []byte("private blob for exclusion")
	privUUID := storeTestBlob(t, r, privData)
	privRid, _ := blob.Exists(r.DB(), privUUID)
	content.MakePrivate(r.DB(), int64(privRid))

	// Pull without send-private pragma — private blob should be excluded.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
	)

	igotUUIDs := make(map[string]bool)
	for _, c := range resp.Cards {
		if ig, ok := c.(*xfer.IGotCard); ok {
			igotUUIDs[ig.UUID] = true
		}
	}
	if !igotUUIDs[pubUUID] {
		t.Error("public blob missing from igots")
	}
	if igotUUIDs[privUUID] {
		t.Error("private blob should be excluded from igots without send-private")
	}
}

func TestEmitIGotsIncludesPrivateWhenAuthorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")

	pubUUID := storeTestBlob(t, r, []byte("public blob auth"))
	privData := []byte("private blob auth")
	privUUID := storeTestBlob(t, r, privData)
	privRid, _ := blob.Exists(r.DB(), privUUID)
	content.MakePrivate(r.DB(), int64(privRid))

	// Pull WITH send-private pragma and x capability.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
	)

	pubFound := false
	privFound := false
	for _, c := range resp.Cards {
		if ig, ok := c.(*xfer.IGotCard); ok {
			if ig.UUID == pubUUID {
				pubFound = true
			}
			if ig.UUID == privUUID {
				if !ig.IsPrivate {
					t.Error("private blob igot should have IsPrivate=true")
				}
				privFound = true
			}
		}
	}
	if !pubFound {
		t.Error("public blob missing from igots")
	}
	if !privFound {
		t.Error("private blob should be included when send-private is authorized")
	}
}

func TestHandlerIGotPrivate_Authorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")
	unknownUUID := "dddddddddddddddddddddddddddddddddddddd"

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
		&xfer.IGotCard{UUID: unknownUUID, IsPrivate: true},
	)

	gimmes := findCards[*xfer.GimmeCard](resp)
	found := false
	for _, g := range gimmes {
		if g.UUID == unknownUUID {
			found = true
		}
	}
	if !found {
		t.Error("authorized private igot should produce gimme")
	}
}

func TestHandlerIGotPrivate_Unauthorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oi' WHERE login='nobody'")
	unknownUUID := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.IGotCard{UUID: unknownUUID, IsPrivate: true},
	)

	gimmes := findCards[*xfer.GimmeCard](resp)
	for _, g := range gimmes {
		if g.UUID == unknownUUID {
			t.Error("unauthorized private igot should NOT produce gimme")
		}
	}
}

func TestHandlerIGotDoesNotChangeServerPrivateStatus(t *testing.T) {
	r := setupSyncTestRepo(t)
	// Store a blob and mark it private.
	data := []byte("igot does not change server private status")
	uuid := storeTestBlob(t, r, data)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))
	if !content.IsPrivate(r.DB(), int64(rid)) {
		t.Fatal("precondition: blob should be private")
	}

	// Client sends a public igot for the existing blob.
	// Server is authoritative — this should NOT change the server's private status.
	handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.IGotCard{UUID: uuid, IsPrivate: false},
	)

	if !content.IsPrivate(r.DB(), int64(rid)) {
		t.Error("server private status should not be changed by client igot")
	}
}

func TestHandleGimmePrivate_Authorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")
	data := []byte("gimme private blob")
	uuid := storeTestBlob(t, r, data)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
		&xfer.GimmeCard{UUID: uuid},
	)

	// Should get PrivateCard followed by FileCard.
	files := findCards[*xfer.FileCard](resp)
	found := false
	for _, f := range files {
		if f.UUID == uuid {
			found = true
		}
	}
	if !found {
		t.Error("authorized gimme for private blob should return file")
	}
	privCards := findCards[*xfer.PrivateCard](resp)
	if len(privCards) == 0 {
		t.Error("expected PrivateCard prefix before private file")
	}
}

func TestHandleGimmePrivate_Unauthorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='oi' WHERE login='nobody'")
	data := []byte("gimme private unauthorized")
	uuid := storeTestBlob(t, r, data)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))

	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.GimmeCard{UUID: uuid},
	)

	files := findCards[*xfer.FileCard](resp)
	for _, f := range files {
		if f.UUID == uuid {
			t.Error("unauthorized gimme for private blob should NOT return file")
		}
	}
}

func TestEmitCloneBatchSkipsPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)
	pubData := []byte("clone public blob")
	pubUUID := storeTestBlob(t, r, pubData)
	privData := []byte("clone private blob")
	privUUID := storeTestBlob(t, r, privData)
	privRid, _ := blob.Exists(r.DB(), privUUID)
	content.MakePrivate(r.DB(), int64(privRid))

	resp := handleReq(t, r,
		&xfer.CloneCard{Version: 1},
	)

	files := findCards[*xfer.FileCard](resp)
	fileUUIDs := make(map[string]bool)
	for _, f := range files {
		fileUUIDs[f.UUID] = true
	}
	if !fileUUIDs[pubUUID] {
		t.Error("public blob missing from clone batch")
	}
	if fileUUIDs[privUUID] {
		t.Error("private blob should be excluded from clone batch without send-private")
	}
}

func TestEmitCloneBatchIncludesPrivateWhenAuthorized(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().Exec("UPDATE user SET cap='gx' WHERE login='nobody'")
	pubData := []byte("clone pub auth")
	pubUUID := storeTestBlob(t, r, pubData)
	privData := []byte("clone priv auth")
	privUUID := storeTestBlob(t, r, privData)
	privRid, _ := blob.Exists(r.DB(), privUUID)
	content.MakePrivate(r.DB(), int64(privRid))

	resp := handleReq(t, r,
		&xfer.CloneCard{Version: 1},
		&xfer.PragmaCard{Name: "send-private"},
	)

	files := findCards[*xfer.FileCard](resp)
	fileUUIDs := make(map[string]bool)
	for _, f := range files {
		fileUUIDs[f.UUID] = true
	}
	if !fileUUIDs[pubUUID] {
		t.Error("public blob missing from clone batch")
	}
	if !fileUUIDs[privUUID] {
		t.Error("private blob should be included when send-private authorized")
	}
	privCards := findCards[*xfer.PrivateCard](resp)
	if len(privCards) == 0 {
		t.Error("expected PrivateCard prefix for private blob in clone batch")
	}
}

func TestHandlerIGotFiltersEmit(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 3 blobs with known UUIDs.
	data1 := []byte("igot-filter-blob-one")
	data2 := []byte("igot-filter-blob-two")
	data3 := []byte("igot-filter-blob-three")
	uuid1 := storeTestBlob(t, r, data1)
	uuid2 := storeTestBlob(t, r, data2)
	uuid3 := storeTestBlob(t, r, data3)

	// Client announces it already has blob 1 and blob 2.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.IGotCard{UUID: uuid1},
		&xfer.IGotCard{UUID: uuid2},
	)

	igots := findCards[*xfer.IGotCard](resp)
	igotUUIDs := make(map[string]bool)
	for _, ig := range igots {
		igotUUIDs[ig.UUID] = true
	}

	// Server should NOT echo back blobs the client already has.
	if igotUUIDs[uuid1] {
		t.Errorf("server should not emit igot for uuid1 (%s) — client already has it", uuid1)
	}
	if igotUUIDs[uuid2] {
		t.Errorf("server should not emit igot for uuid2 (%s) — client already has it", uuid2)
	}

	// Server SHOULD emit igot for blob 3, which the client didn't announce.
	if !igotUUIDs[uuid3] {
		t.Errorf("server should emit igot for uuid3 (%s) — client doesn't have it", uuid3)
	}
}

func TestSendAllClustersExcludesPrivate(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Store 200 blobs so clustering triggers.
	for i := 0; i < 200; i++ {
		data := []byte(fmt.Sprintf("cluster-priv-blob-%04d", i))
		if _, _, err := blob.Store(r.DB(), data); err != nil {
			t.Fatalf("Store blob %d: %v", i, err)
		}
	}

	n, err := content.GenerateClusters(r.DB())
	if err != nil {
		t.Fatalf("GenerateClusters: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least 1 cluster")
	}

	// Mark the cluster artifact itself as private.
	var clusterRid int
	err = r.DB().QueryRow(`
		SELECT tx.rid FROM tagxref tx
		WHERE tx.tagid = 7
		LIMIT 1`,
	).Scan(&clusterRid)
	if err != nil {
		t.Fatalf("find cluster rid: %v", err)
	}
	content.MakePrivate(r.DB(), int64(clusterRid))

	resp, err := HandleSync(context.Background(), r, &xfer.Message{
		Cards: []xfer.Card{
			&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
			&xfer.PragmaCard{Name: "req-clusters"},
		},
	})
	if err != nil {
		t.Fatalf("HandleSync: %v", err)
	}

	// The private cluster should not appear in igots.
	var clusterUUID string
	r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", clusterRid).Scan(&clusterUUID)

	for _, c := range resp.Cards {
		if ig, ok := c.(*xfer.IGotCard); ok && ig.UUID == clusterUUID {
			t.Error("private cluster blob should be excluded from sendAllClusters")
		}
	}
}

func TestHandleGimmeWithContentCache(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("cached gimme blob")
	uuid := storeTestBlob(t, r, data)

	cache := content.NewCache(1 << 20)

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.GimmeCard{UUID: uuid},
	}}
	resp, err := HandleSyncWithOpts(context.Background(), r, req, HandleOpts{
		ContentCache: cache,
	})
	if err != nil {
		t.Fatalf("HandleSyncWithOpts: %v", err)
	}

	files := findCards[*xfer.FileCard](resp)
	found := false
	for _, f := range files {
		if f.UUID == uuid && string(f.Content) == string(data) {
			found = true
		}
	}
	if !found {
		t.Fatal("expected file card with correct content via cached handler")
	}

	// Cache should have been populated.
	stats := cache.Stats()
	if stats.Misses == 0 {
		t.Fatal("expected at least 1 cache miss (the initial expand)")
	}

	// Second request for the same blob should hit cache.
	resp2, err := HandleSyncWithOpts(context.Background(), r, req, HandleOpts{
		ContentCache: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	files2 := findCards[*xfer.FileCard](resp2)
	found2 := false
	for _, f := range files2 {
		if f.UUID == uuid && string(f.Content) == string(data) {
			found2 = true
		}
	}
	if !found2 {
		t.Fatal("expected file card on second request (cache hit path)")
	}

	stats2 := cache.Stats()
	if stats2.Hits == 0 {
		t.Fatal("expected cache hit on second handler call")
	}
}

func TestSyncRoundTripWithContentCache(t *testing.T) {
	server := setupSyncTestRepo(t)
	client := setupSyncTestRepo(t)

	// Store a blob on the server.
	data := []byte("sync round trip cached data")
	serverUUID := storeTestBlob(t, server, data)

	cache := content.NewCache(1 << 20)

	transport := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, err := HandleSyncWithOpts(context.Background(), server, req, HandleOpts{
				ContentCache: cache,
			})
			if err != nil {
				t.Fatalf("HandleSyncWithOpts: %v", err)
			}
			return resp
		},
	}

	result, err := Sync(context.Background(), client, transport, SyncOpts{
		Pull:         true,
		ContentCache: cache,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.FilesRecvd == 0 {
		t.Fatal("expected to receive files")
	}

	// Verify client has the blob.
	rid, ok := blob.Exists(client.DB(), serverUUID)
	if !ok {
		t.Fatal("blob not found on client after sync")
	}
	got, err := content.Expand(client.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", got, data)
	}

	// Cache should have recorded activity.
	stats := cache.Stats()
	if stats.Hits+stats.Misses == 0 {
		t.Fatal("cache was never used during sync")
	}
}

func TestHandlerIGotFiltersPrivateEmit(t *testing.T) {
	r := setupSyncTestRepo(t)
	// Grant private sync capability.
	r.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")

	// Store a blob and mark it private.
	data := []byte("igot-filter-private-blob")
	uuid := storeTestBlob(t, r, data)
	rid, _ := blob.Exists(r.DB(), uuid)
	content.MakePrivate(r.DB(), int64(rid))

	// Store a second private blob the client does NOT have.
	data2 := []byte("igot-filter-private-blob-two")
	uuid2 := storeTestBlob(t, r, data2)
	rid2, _ := blob.Exists(r.DB(), uuid2)
	content.MakePrivate(r.DB(), int64(rid2))

	// Client announces it has the first private blob.
	resp := handleReq(t, r,
		&xfer.PullCard{ServerCode: "s", ProjectCode: "p"},
		&xfer.PragmaCard{Name: "send-private"},
		&xfer.IGotCard{UUID: uuid, IsPrivate: true},
	)

	igots := findCards[*xfer.IGotCard](resp)
	for _, ig := range igots {
		if ig.UUID == uuid {
			t.Errorf("server should not emit private igot for %s — client already has it", uuid)
		}
	}

	// The second private blob should still appear.
	found := false
	for _, ig := range igots {
		if ig.UUID == uuid2 && ig.IsPrivate {
			found = true
		}
	}
	if !found {
		t.Errorf("server should emit private igot for %s — client doesn't have it", uuid2)
	}
}
