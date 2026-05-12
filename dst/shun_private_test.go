package dst

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/internal/shun"
	libsync "github.com/danmestas/libfossil/internal/sync"
	"github.com/danmestas/libfossil/simio"

	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// TestShunnedBlobDoesNotPropagate verifies that shunned and private blobs
// are excluded from sync, while normal blobs propagate correctly.
func TestShunnedBlobDoesNotPropagate(t *testing.T) {
	tmpDir := t.TempDir()

	// Create server repo and seed blobs.
	serverPath := filepath.Join(tmpDir, "server.fossil")
	serverRepo, err := repo.Create(serverPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("create server repo: %v", err)
	}
	t.Cleanup(func() { serverRepo.Close() })

	// Store normal blob.
	_, normalUUID, err := blob.Store(serverRepo.DB(), []byte("normal-blob-content"))
	if err != nil {
		t.Fatalf("store normal blob: %v", err)
	}

	// Store shunned blob.
	_, shunnedUUID, err := blob.Store(serverRepo.DB(), []byte("shunned-blob-content"))
	if err != nil {
		t.Fatalf("store shunned blob: %v", err)
	}
	if err := shun.Add(serverRepo.DB(), shunnedUUID, "test shun"); err != nil {
		t.Fatalf("shun.Add: %v", err)
	}

	// Store private blob.
	privRid, _, err := blob.Store(serverRepo.DB(), []byte("private-blob-content"))
	if err != nil {
		t.Fatalf("store private blob: %v", err)
	}
	if err := content.MakePrivate(serverRepo.DB(), int64(privRid)); err != nil {
		t.Fatalf("MakePrivate: %v", err)
	}
	var privateUUID string
	if err := serverRepo.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", privRid).Scan(&privateUUID); err != nil {
		t.Fatalf("query private UUID: %v", err)
	}

	// Read server's project-code and server-code.
	var projCode, srvCode string
	serverRepo.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projCode)
	serverRepo.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&srvCode)

	// Create client repo with matching project-code.
	clientPath := filepath.Join(tmpDir, "client.fossil")
	clientRepo, err := repo.Create(clientPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("create client repo: %v", err)
	}
	t.Cleanup(func() { clientRepo.Close() })
	clientRepo.DB().Exec("UPDATE config SET value=? WHERE name='project-code'", projCode)

	// Sync client from server via MockFossil.
	mf := NewMockFossil(serverRepo)
	ctx := context.Background()

	for round := 0; round < 10; round++ {
		result, err := libsync.Sync(ctx, clientRepo, mf, libsync.SyncOpts{
			Pull:        true,
			ProjectCode: projCode,
			ServerCode:  srvCode,
		})
		if err != nil {
			t.Fatalf("sync round %d: %v", round, err)
		}
		if result.FilesSent == 0 && result.FilesRecvd == 0 {
			t.Logf("converged at round %d", round)
			break
		}
	}

	// Assert: client HAS the normal blob.
	if !HasBlob(clientRepo, normalUUID) {
		t.Error("client should have the normal blob")
	}

	// Assert: client does NOT have the shunned blob.
	if HasBlob(clientRepo, shunnedUUID) {
		t.Error("client should NOT have the shunned blob")
	}

	// Assert: client does NOT have the private blob.
	if HasBlob(clientRepo, privateUUID) {
		t.Error("client should NOT have the private blob")
	}
}

// TestPrivateBlobPropagatesWithFlag verifies that private blobs propagate
// when Private=true is set in SyncOpts and the server grants 'x' capability,
// while shunned blobs are always excluded regardless.
func TestPrivateBlobPropagatesWithFlag(t *testing.T) {
	tmpDir := t.TempDir()

	// Create server repo and seed blobs.
	serverPath := filepath.Join(tmpDir, "server.fossil")
	serverRepo, err := repo.Create(serverPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("create server repo: %v", err)
	}
	t.Cleanup(func() { serverRepo.Close() })

	// Grant 'x' capability to nobody for private sync.
	serverRepo.DB().Exec("UPDATE user SET cap='oix' WHERE login='nobody'")

	// Store normal blob.
	_, normalUUID, err := blob.Store(serverRepo.DB(), []byte("normal-blob-content"))
	if err != nil {
		t.Fatalf("store normal blob: %v", err)
	}

	// Store shunned blob.
	_, shunnedUUID, err := blob.Store(serverRepo.DB(), []byte("shunned-blob-content"))
	if err != nil {
		t.Fatalf("store shunned blob: %v", err)
	}
	if err := shun.Add(serverRepo.DB(), shunnedUUID, "test shun"); err != nil {
		t.Fatalf("shun.Add: %v", err)
	}

	// Store private blob.
	privRid, _, err := blob.Store(serverRepo.DB(), []byte("private-blob-content"))
	if err != nil {
		t.Fatalf("store private blob: %v", err)
	}
	if err := content.MakePrivate(serverRepo.DB(), int64(privRid)); err != nil {
		t.Fatalf("MakePrivate: %v", err)
	}
	var privateUUID string
	if err := serverRepo.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", privRid).Scan(&privateUUID); err != nil {
		t.Fatalf("query private UUID: %v", err)
	}

	// Read server's project-code and server-code.
	var projCode, srvCode string
	serverRepo.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projCode)
	serverRepo.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&srvCode)

	// Create client repo with matching project-code.
	clientPath := filepath.Join(tmpDir, "client.fossil")
	clientRepo, err := repo.Create(clientPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("create client repo: %v", err)
	}
	t.Cleanup(func() { clientRepo.Close() })
	clientRepo.DB().Exec("UPDATE config SET value=? WHERE name='project-code'", projCode)

	// Sync client from server with Private=true.
	mf := NewMockFossil(serverRepo)
	ctx := context.Background()

	for round := 0; round < 10; round++ {
		result, err := libsync.Sync(ctx, clientRepo, mf, libsync.SyncOpts{
			Pull:        true,
			Private:     true,
			ProjectCode: projCode,
			ServerCode:  srvCode,
		})
		if err != nil {
			t.Fatalf("sync round %d: %v", round, err)
		}
		if result.FilesSent == 0 && result.FilesRecvd == 0 {
			t.Logf("converged at round %d", round)
			break
		}
	}

	// Assert: client HAS the normal blob.
	if !HasBlob(clientRepo, normalUUID) {
		t.Error("client should have the normal blob")
	}

	// Assert: client does NOT have the shunned blob (shun always blocks).
	if HasBlob(clientRepo, shunnedUUID) {
		t.Error("client should NOT have the shunned blob")
	}

	// Assert: client DOES have the private blob (Private flag allows it).
	if !HasBlob(clientRepo, privateUUID) {
		t.Error("client should have the private blob when Private=true")
	}
}
