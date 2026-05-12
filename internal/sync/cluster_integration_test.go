package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/xfer"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// TestPushOnly_DrainsServerGimmes verifies that when the client pushes
// without pulling (SyncOpts{Push:true, Pull:false}), the server still
// requests blobs it does not have via gimme cards and the client services
// them across multiple rounds.
//
// Regression: in v0.4.0 the server only emitted gimme cards when pullOK
// was true (driven by the client's pull card). Push-only clients exited
// after one round even when the server was missing artifacts, leaving
// the server with an incomplete blob store.
func TestPushOnly_DrainsServerGimmes(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	clientRepo, err := repo.Create(clientPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create client: %v", err)
	}
	t.Cleanup(func() { clientRepo.Close() })

	const numBlobs = 50
	for i := range numBlobs {
		content := []byte(fmt.Sprintf("push-only-blob-%04d", i))
		if _, _, err := blob.Store(clientRepo.DB(), content); err != nil {
			t.Fatalf("blob.Store[%d]: %v", i, err)
		}
	}

	serverPath := filepath.Join(t.TempDir(), "server.fossil")
	serverRepo, err := repo.Create(serverPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create server: %v", err)
	}
	t.Cleanup(func() { serverRepo.Close() })

	transport := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, err := HandleSync(context.Background(), serverRepo, req)
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			return resp
		},
	}

	var projCode, srvCode string
	clientRepo.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projCode)
	clientRepo.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&srvCode)

	result, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Push:        true,
		Pull:        false,
		ProjectCode: projCode,
		ServerCode:  srvCode,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var serverCount int
	if err := serverRepo.DB().QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&serverCount); err != nil {
		t.Fatalf("count blobs: %v", err)
	}

	t.Logf("push-only sync completed: rounds=%d sent=%d server_blobs=%d",
		result.Rounds, result.FilesSent, serverCount)

	if serverCount < numBlobs {
		t.Fatalf("server has %d blobs, want >= %d (push-only did not drain server gimmes)",
			serverCount, numBlobs)
	}
	if result.FilesSent < numBlobs {
		t.Fatalf("FilesSent = %d, want >= %d (multi-round push-only failed to send all)",
			result.FilesSent, numBlobs)
	}
	// Multi-round behaviour: the protocol needs at least 2 rounds (igot then file).
	if result.Rounds < 2 {
		t.Fatalf("Rounds = %d, want >= 2 (push-only converged before draining gimmes)",
			result.Rounds)
	}
}

// TestClusterSync_RoundTrip creates a client repo with 200 blobs, syncs them
// to an empty server repo via MockTransport wrapping HandleSync, and verifies
// all 200 blobs arrive within 10 rounds.
func TestClusterSync_RoundTrip(t *testing.T) {
	// --- Setup: client repo with 200 blobs ---
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	clientRepo, err := repo.Create(clientPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create client: %v", err)
	}
	t.Cleanup(func() { clientRepo.Close() })

	for i := range 200 {
		content := []byte(fmt.Sprintf("cluster-roundtrip-blob-%04d", i))
		_, _, err := blob.Store(clientRepo.DB(), content)
		if err != nil {
			t.Fatalf("blob.Store[%d]: %v", i, err)
		}
	}

	// --- Setup: empty server repo ---
	serverPath := filepath.Join(t.TempDir(), "server.fossil")
	serverRepo, err := repo.Create(serverPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create server: %v", err)
	}
	t.Cleanup(func() { serverRepo.Close() })

	// --- Transport: MockTransport wrapping HandleSync ---
	transport := &MockTransport{
		Handler: func(req *xfer.Message) *xfer.Message {
			resp, err := HandleSync(context.Background(), serverRepo, req)
			if err != nil {
				t.Fatalf("HandleSync: %v", err)
			}
			return resp
		},
	}

	// --- Sync: client pushes to server ---
	var projCode, srvCode string
	clientRepo.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projCode)
	clientRepo.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&srvCode)

	result, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Push:        true,
		Pull:        true,
		ProjectCode: projCode,
		ServerCode:  srvCode,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// --- Verify: all 200 blobs arrived ---
	var serverCount int
	err = serverRepo.DB().QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&serverCount)
	if err != nil {
		t.Fatalf("count blobs: %v", err)
	}

	t.Logf("sync completed: rounds=%d sent=%d recv=%d server_blobs=%d",
		result.Rounds, result.FilesSent, result.FilesRecvd, serverCount)

	if serverCount < 200 {
		t.Fatalf("server has %d blobs, want >= 200", serverCount)
	}

	// --- Verify: converged within 10 rounds ---
	if result.Rounds > 10 {
		t.Fatalf("took %d rounds, want <= 10", result.Rounds)
	}
}
