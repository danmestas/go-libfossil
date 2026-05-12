package sync

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/xfer"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func TestMockTransportExchange(t *testing.T) {
	mt := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			return &xfer.Message{Cards: []xfer.Card{
				&xfer.IGotCard{UUID: "response-uuid"},
			}}
		},
	}
	resp, err := mt.Exchange(context.Background(), &xfer.Message{})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(resp.Cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(resp.Cards))
	}
}

func TestMockTransportNilHandler(t *testing.T) {
	mt := &MockTransport{}
	resp, err := mt.Exchange(context.Background(), &xfer.Message{})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(resp.Cards) != 0 {
		t.Fatalf("expected empty response")
	}
}

func sha1hex(data string) string {
	h := sha1.Sum([]byte(data))
	return hex.EncodeToString(h[:])
}

func TestComputeLogin(t *testing.T) {
	payload := []byte("pull abc def\nigot da39a3ee5e6b4b0d3255bfef95601890afd80709\n")
	card := computeLogin("testuser", "secret", "projcode", payload)
	if card.User != "testuser" {
		t.Fatalf("User = %q", card.User)
	}
	expectedNonce := sha1hex(string(payload))
	if card.Nonce != expectedNonce {
		t.Fatalf("Nonce = %q, want %q", card.Nonce, expectedNonce)
	}
	sharedSecret := sha1hex("projcode/testuser/secret")
	expectedSig := sha1hex(card.Nonce + sharedSecret)
	if card.Signature != expectedSig {
		t.Fatalf("Signature = %q, want %q", card.Signature, expectedSig)
	}
}

func TestComputeLoginAnonymous(t *testing.T) {
	card := computeLogin("anonymous", "", "projcode", []byte("test\n"))
	if card.User != "anonymous" {
		t.Fatalf("User = %q", card.User)
	}
	if card.Nonce == "" {
		t.Fatal("anonymous should still have nonce")
	}
}

func TestAppendRandomComment(t *testing.T) {
	rng := simio.CryptoRand{}
	payload1 := appendRandomComment([]byte("test\n"), rng)
	payload2 := appendRandomComment([]byte("test\n"), rng)
	if string(payload1) == string(payload2) {
		t.Fatal("random comments should be unique")
	}
	if payload1[len(payload1)-1] != '\n' {
		t.Fatal("should end with newline")
	}
}

// --- Test helpers ---

func setupSyncTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func newTestSession(t *testing.T, opts SyncOpts) (*session, *repo.Repo) {
	t.Helper()
	r := setupSyncTestRepo(t)
	return newSession(r, opts), r
}

// --- buildRequest tests ---

func TestBuildRequestPushOnly(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{
		Push:        true,
		Pull:        false,
		ServerCode:  "sc",
		ProjectCode: "pc",
	})
	msg, err := s.buildRequest(0)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	pushCards := cardsByType(msg, xfer.CardPush)
	pullCards := cardsByType(msg, xfer.CardPull)
	if len(pushCards) != 1 {
		t.Fatalf("push cards = %d, want 1", len(pushCards))
	}
	if len(pullCards) != 0 {
		t.Fatalf("pull cards = %d, want 0", len(pullCards))
	}
}

func TestBuildRequestPullOnly(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{
		Push:        false,
		Pull:        true,
		ServerCode:  "sc",
		ProjectCode: "pc",
	})
	msg, err := s.buildRequest(0)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	pushCards := cardsByType(msg, xfer.CardPush)
	pullCards := cardsByType(msg, xfer.CardPull)
	if len(pushCards) != 0 {
		t.Fatalf("push cards = %d, want 0", len(pushCards))
	}
	if len(pullCards) != 1 {
		t.Fatalf("pull cards = %d, want 1", len(pullCards))
	}
}

func TestBuildRequestHasPragmaClientVersion(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})
	msg, err := s.buildRequest(0)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	pragmas := cardsByType(msg, xfer.CardPragma)
	if len(pragmas) == 0 {
		t.Fatal("no pragma cards found")
	}
	p := pragmas[0].(*xfer.PragmaCard)
	if p.Name != "client-version" {
		t.Fatalf("pragma name = %q, want client-version", p.Name)
	}
}

func TestBuildRequestIGotFromUnclustered(t *testing.T) {
	s, r := newTestSession(t, SyncOpts{Push: true, ServerCode: "sc", ProjectCode: "pc"})

	// Store a blob — blob.Store auto-marks unclustered.
	content := []byte("test artifact for igot")
	_, uuid, err := blob.Store(r.DB(), content)
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}

	msg, err := s.buildRequest(0)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	igots := cardsByType(msg, xfer.CardIGot)
	if len(igots) == 0 {
		t.Fatal("expected igot cards from unclustered")
	}
	found := false
	for _, c := range igots {
		if c.(*xfer.IGotCard).UUID == uuid {
			found = true
		}
	}
	if !found {
		t.Fatalf("igot card for uuid %s not found", uuid)
	}
}

func TestBuildRequestGimmeForPhantoms(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})

	phantomUUID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s.phantoms[phantomUUID] = true

	msg, err := s.buildRequest(0)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	gimmes := cardsByType(msg, xfer.CardGimme)
	if len(gimmes) != 1 {
		t.Fatalf("gimme cards = %d, want 1", len(gimmes))
	}
	if gimmes[0].(*xfer.GimmeCard).UUID != phantomUUID {
		t.Fatalf("gimme uuid = %q, want %q", gimmes[0].(*xfer.GimmeCard).UUID, phantomUUID)
	}
}

func TestBuildRequestFileForPendingSend(t *testing.T) {
	s, r := newTestSession(t, SyncOpts{Push: true, ServerCode: "sc", ProjectCode: "pc"})

	// Store a blob and add its UUID to pendingSend
	content := []byte("file to send over sync")
	_, uuid, err := blob.Store(r.DB(), content)
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	s.pendingSend[uuid] = true

	msg, err := s.buildRequest(0)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	files := cardsByType(msg, xfer.CardFile)
	if len(files) != 1 {
		t.Fatalf("file cards = %d, want 1", len(files))
	}
	fc := files[0].(*xfer.FileCard)
	if fc.UUID != uuid {
		t.Fatalf("file uuid = %q, want %q", fc.UUID, uuid)
	}
	if string(fc.Content) != string(content) {
		t.Fatalf("file content mismatch")
	}
}

func TestBuildRequestWithLogin(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{
		Pull:        true,
		ServerCode:  "sc",
		ProjectCode: "pc",
		User:        "alice",
		Password:    "pass123",
	})
	msg, err := s.buildRequest(0)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	// Login card should be first
	if len(msg.Cards) == 0 {
		t.Fatal("no cards")
	}
	login, ok := msg.Cards[0].(*xfer.LoginCard)
	if !ok {
		t.Fatalf("first card is %T, want *LoginCard", msg.Cards[0])
	}
	if login.User != "alice" {
		t.Fatalf("login.User = %q, want alice", login.User)
	}
	if login.Nonce == "" {
		t.Fatal("login.Nonce is empty")
	}
	if login.Signature == "" {
		t.Fatal("login.Signature is empty")
	}
}

// --- processResponse tests ---

func TestProcessResponseStoresFileCard(t *testing.T) {
	s, r := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})

	content := []byte("artifact from server")
	uuid := hash.SHA1(content)

	resp := &xfer.Message{Cards: []xfer.Card{
		&xfer.FileCard{UUID: uuid, Content: content},
	}}
	done, err := s.processResponse(resp)
	if err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if done {
		t.Fatal("should not be done (just received a file)")
	}

	// Verify blob was stored
	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatalf("blob %s not found after processResponse", uuid)
	}
	if rid <= 0 {
		t.Fatalf("rid = %d, want > 0", rid)
	}
	if s.result.FilesRecvd != 1 {
		t.Fatalf("FilesRecvd = %d, want 1", s.result.FilesRecvd)
	}
}

func TestProcessResponseIGotAddsPhantom(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})

	missingUUID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	resp := &xfer.Message{Cards: []xfer.Card{
		&xfer.IGotCard{UUID: missingUUID},
	}}
	_, err := s.processResponse(resp)
	if err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if !s.remoteHas[missingUUID] {
		t.Fatal("remoteHas should contain the UUID")
	}
	if !s.phantoms[missingUUID] {
		t.Fatal("phantoms should contain the UUID (pull=true, blob missing)")
	}
}

func TestProcessResponseIGotNoPhantomWhenExists(t *testing.T) {
	s, r := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})

	// Store a blob so it exists locally
	content := []byte("already have this")
	_, uuid, err := blob.Store(r.DB(), content)
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}

	resp := &xfer.Message{Cards: []xfer.Card{
		&xfer.IGotCard{UUID: uuid},
	}}
	_, err = s.processResponse(resp)
	if err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if !s.remoteHas[uuid] {
		t.Fatal("remoteHas should contain the UUID")
	}
	if s.phantoms[uuid] {
		t.Fatal("phantoms should NOT contain the UUID (we already have it)")
	}
}

func TestProcessResponseGimmeAddsPendingSend(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{Push: true, ServerCode: "sc", ProjectCode: "pc"})

	wantUUID := "cccccccccccccccccccccccccccccccccccccccc"
	resp := &xfer.Message{Cards: []xfer.Card{
		&xfer.GimmeCard{UUID: wantUUID},
	}}
	done, err := s.processResponse(resp)
	if err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if done {
		t.Fatal("should not be done (gimme received)")
	}
	if !s.pendingSend[wantUUID] {
		t.Fatal("pendingSend should contain the UUID")
	}
}

func TestProcessResponseCookieCached(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})

	resp := &xfer.Message{Cards: []xfer.Card{
		&xfer.CookieCard{Value: "session-abc-123"},
	}}
	_, err := s.processResponse(resp)
	if err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if s.cookie != "session-abc-123" {
		t.Fatalf("cookie = %q, want session-abc-123", s.cookie)
	}
}

func TestProcessResponseConvergence(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})

	// Empty response with no pending work = converged
	resp := &xfer.Message{}
	done, err := s.processResponse(resp)
	if err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if !done {
		t.Fatal("should be done (empty response, no pending work)")
	}
}

func TestProcessResponseErrorCard(t *testing.T) {
	s, _ := newTestSession(t, SyncOpts{Pull: true, ServerCode: "sc", ProjectCode: "pc"})

	resp := &xfer.Message{Cards: []xfer.Card{
		&xfer.ErrorCard{Message: "access denied"},
	}}
	done, err := s.processResponse(resp)
	if err != nil {
		t.Fatalf("processResponse: %v", err)
	}
	if !done {
		t.Fatal("should be done (only error, no work to do)")
	}
	if len(s.result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(s.result.Errors))
	}
	if s.result.Errors[0] != "error: access denied" {
		t.Fatalf("error = %q", s.result.Errors[0])
	}
}

// --- Sync convergence loop tests ---

func TestSyncSingleRound(t *testing.T) {
	r := setupSyncTestRepo(t)
	mt := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			return &xfer.Message{} // empty = converge
		},
	}
	result, err := Sync(context.Background(), r, mt, SyncOpts{
		Pull:        true,
		ServerCode:  "sc",
		ProjectCode: "pc",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Rounds != 1 {
		t.Fatalf("Rounds = %d, want 1", result.Rounds)
	}
}

func TestSyncMultiRound(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Content for the file we'll receive in round 2
	fileContent := []byte("artifact from multi-round sync")
	fileUUID := hash.SHA1(fileContent)

	round := 0
	mt := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			round++
			switch round {
			case 1:
				// Round 1: server says "igot" for a blob we don't have
				return &xfer.Message{Cards: []xfer.Card{
					&xfer.IGotCard{UUID: fileUUID},
				}}
			case 2:
				// Round 2: server sends the file we asked for
				return &xfer.Message{Cards: []xfer.Card{
					&xfer.FileCard{UUID: fileUUID, Content: fileContent},
				}}
			default:
				// Round 3+: empty = converge
				return &xfer.Message{}
			}
		},
	}
	result, err := Sync(context.Background(), r, mt, SyncOpts{
		Pull:        true,
		ServerCode:  "sc",
		ProjectCode: "pc",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Rounds != 3 {
		t.Fatalf("Rounds = %d, want 3", result.Rounds)
	}
	if result.FilesRecvd != 1 {
		t.Fatalf("FilesRecvd = %d, want 1", result.FilesRecvd)
	}
	// Verify the blob ended up in the repo
	_, ok := blob.Exists(r.DB(), fileUUID)
	if !ok {
		t.Fatal("file not stored after multi-round sync")
	}
}

func TestSyncContextCancellation(t *testing.T) {
	r := setupSyncTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	mt := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			return &xfer.Message{}
		},
	}
	_, err := Sync(ctx, r, mt, SyncOpts{
		Pull:        true,
		ServerCode:  "sc",
		ProjectCode: "pc",
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestSyncMaxRoundsExceeded(t *testing.T) {
	r := setupSyncTestRepo(t)

	// Mock that never converges: always returns a gimme card
	mt := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			return &xfer.Message{Cards: []xfer.Card{
				&xfer.GimmeCard{UUID: "dddddddddddddddddddddddddddddddddddddddd"},
			}}
		},
	}
	result, err := Sync(context.Background(), r, mt, SyncOpts{
		Push:        true,
		ServerCode:  "sc",
		ProjectCode: "pc",
	})
	if err == nil {
		t.Fatal("expected error for max rounds exceeded")
	}
	if result.Rounds != MaxRounds {
		t.Fatalf("Rounds = %d, want %d", result.Rounds, MaxRounds)
	}
}

// --- Benchmarks ---

// BenchmarkBuildRequest benchmarks buildRequest with 100 unclustered artifacts.
func BenchmarkBuildRequest(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.fossil")
	r, err := repo.Create(path, "benchuser", simio.CryptoRand{}, "")
	if err != nil {
		b.Fatalf("repo.Create: %v", err)
	}
	defer r.Close()

	// Populate 100 unclustered artifacts
	for i := 0; i < 100; i++ {
		content := []byte(fmt.Sprintf("benchmark artifact %d", i))
		_, _, err := blob.Store(r.DB(), content)
		if err != nil {
			b.Fatalf("blob.Store: %v", err)
		}
	}

	s := newSession(r, SyncOpts{
		Push:        true,
		Pull:        true,
		ServerCode:  "sc",
		ProjectCode: "pc",
		User:        "alice",
		Password:    "secret",
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := s.buildRequest(0)
		if err != nil {
			b.Fatalf("buildRequest: %v", err)
		}
	}
}

// BenchmarkProcessResponse benchmarks processResponse with 50 file cards.
func BenchmarkProcessResponse(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.fossil")
	r, err := repo.Create(path, "benchuser", simio.CryptoRand{}, "")
	if err != nil {
		b.Fatalf("repo.Create: %v", err)
	}
	defer r.Close()

	// Build a message with 50 file cards
	var cards []xfer.Card
	for i := 0; i < 50; i++ {
		content := []byte(fmt.Sprintf("benchmark file content %d", i))
		uuid := hash.SHA1(content)
		cards = append(cards, &xfer.FileCard{UUID: uuid, Content: content})
	}
	msg := &xfer.Message{Cards: cards}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Each iteration needs a fresh session+repo because processResponse stores blobs
		bpath := filepath.Join(b.TempDir(), fmt.Sprintf("bench-%d.fossil", i))
		br, err := repo.Create(bpath, "benchuser", simio.CryptoRand{}, "")
		if err != nil {
			b.Fatalf("repo.Create: %v", err)
		}
		s := newSession(br, SyncOpts{
			Pull:        true,
			ServerCode:  "sc",
			ProjectCode: "pc",
		})
		b.StartTimer()

		_, err = s.processResponse(msg)
		if err != nil {
			b.Fatalf("processResponse: %v", err)
		}

		b.StopTimer()
		br.Close()
	}
}
