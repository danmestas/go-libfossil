package dst

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/internal/xfer"
	"github.com/danmestas/libfossil/simio"
)

func createMockFossil(t *testing.T) *MockFossil {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.fossil")
	r, err := repo.Create(path, "master", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return NewMockFossil(r)
}

func TestMockFossilStoreArtifact(t *testing.T) {
	mf := createMockFossil(t)

	data := []byte("hello from mock fossil")
	uuid, err := mf.StoreArtifact(data)
	if err != nil {
		t.Fatalf("StoreArtifact: %v", err)
	}
	expected := hash.SHA1(data)
	if uuid != expected {
		t.Fatalf("UUID = %q, want %q", uuid, expected)
	}
}

func TestMockFossilPullFlow(t *testing.T) {
	mf := createMockFossil(t)

	// Seed an artifact in the master.
	data := []byte("artifact for pull test")
	uuid, _ := mf.StoreArtifact(data)

	ctx := context.Background()

	// Round 1: client sends pull — server should respond with igot.
	req1 := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "sc", ProjectCode: "pc"},
	}}
	resp1, err := mf.Exchange(ctx, req1)
	if err != nil {
		t.Fatalf("Exchange round 1: %v", err)
	}

	var foundIGot bool
	for _, c := range resp1.Cards {
		if igot, ok := c.(*xfer.IGotCard); ok && igot.UUID == uuid {
			foundIGot = true
		}
	}
	if !foundIGot {
		t.Fatalf("expected igot card for %s in response", uuid)
	}

	// Round 2: client sends gimme — server should respond with file.
	req2 := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "sc", ProjectCode: "pc"},
		&xfer.GimmeCard{UUID: uuid},
	}}
	resp2, err := mf.Exchange(ctx, req2)
	if err != nil {
		t.Fatalf("Exchange round 2: %v", err)
	}

	var foundFile bool
	for _, c := range resp2.Cards {
		if fc, ok := c.(*xfer.FileCard); ok && fc.UUID == uuid {
			if string(fc.Content) != string(data) {
				t.Fatalf("file content mismatch")
			}
			foundFile = true
		}
	}
	if !foundFile {
		t.Fatalf("expected file card for %s in response", uuid)
	}
}

func TestMockFossilPushFlow(t *testing.T) {
	mf := createMockFossil(t)

	data := []byte("artifact from client push")
	uuid := hash.SHA1(data)

	ctx := context.Background()

	// Round 1: client sends push + igot — server should gimme.
	// HandleSync requires both push AND pull for igot→gimme,
	// since gimme is a "I need this" response to igot.
	req1 := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "sc", ProjectCode: "pc"},
		&xfer.PullCard{ServerCode: "sc", ProjectCode: "pc"},
		&xfer.IGotCard{UUID: uuid},
	}}
	resp1, err := mf.Exchange(ctx, req1)
	if err != nil {
		t.Fatalf("Exchange round 1: %v", err)
	}

	var foundGimme bool
	for _, c := range resp1.Cards {
		if gc, ok := c.(*xfer.GimmeCard); ok && gc.UUID == uuid {
			foundGimme = true
		}
	}
	if !foundGimme {
		t.Fatalf("expected gimme card for %s in response", uuid)
	}

	// Round 2: client sends push + file — server should store it.
	req2 := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "sc", ProjectCode: "pc"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	resp2, err := mf.Exchange(ctx, req2)
	if err != nil {
		t.Fatalf("Exchange round 2: %v", err)
	}

	for _, c := range resp2.Cards {
		if ec, ok := c.(*xfer.ErrorCard); ok {
			t.Fatalf("unexpected error card: %s", ec.Message)
		}
	}

	// Verify the artifact is in the master repo.
	_, ok := blob.Exists(mf.Repo().DB(), uuid)
	if !ok {
		t.Fatal("blob not found after push")
	}
}

func TestMockFossilPushBadUUID(t *testing.T) {
	mf := createMockFossil(t)
	ctx := context.Background()

	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "sc", ProjectCode: "pc"},
		&xfer.FileCard{UUID: "0000000000000000000000000000000000000000", Content: []byte("data")},
	}}
	resp, err := mf.Exchange(ctx, req)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	var foundError bool
	for _, c := range resp.Cards {
		if _, ok := c.(*xfer.ErrorCard); ok {
			foundError = true
		}
	}
	if !foundError {
		t.Fatal("expected error card for UUID mismatch")
	}
}

func TestMockFossilRejectsBadLogin(t *testing.T) {
	mf := createMockFossil(t)
	ctx := context.Background()

	// Bad credentials should produce an auth error.
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.LoginCard{User: "testuser", Nonce: "abc", Signature: "def"},
		&xfer.PullCard{ServerCode: "sc", ProjectCode: "pc"},
	}}
	resp, err := mf.Exchange(ctx, req)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	foundAuthError := false
	for _, c := range resp.Cards {
		if ec, ok := c.(*xfer.ErrorCard); ok && ec.Message == "authentication failed" {
			foundAuthError = true
		}
	}
	if !foundAuthError {
		t.Fatal("expected authentication failed error for bad credentials")
	}
}

func TestMockFossilAnonymousPull(t *testing.T) {
	mf := createMockFossil(t)
	ctx := context.Background()

	// Anonymous (no login card) should work — nobody user has full caps.
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "sc", ProjectCode: "pc"},
	}}
	resp, err := mf.Exchange(ctx, req)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	for _, c := range resp.Cards {
		if ec, ok := c.(*xfer.ErrorCard); ok {
			t.Fatalf("unexpected error: %s", ec.Message)
		}
	}
}
