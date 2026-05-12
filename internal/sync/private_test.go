package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/xfer"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// newPrivateTestPair creates a server and client repo with matching project code.
// The server's nobody user is granted the given capabilities.
func newPrivateTestPair(t *testing.T, nobodyCaps string) (server, client *repo.Repo) {
	t.Helper()
	dir := t.TempDir()

	sPath := filepath.Join(dir, "server.fossil")
	s, err := repo.Create(sPath, "test", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("server repo: %v", err)
	}

	cPath := filepath.Join(dir, "client.fossil")
	c, err := repo.Create(cPath, "test", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("client repo: %v", err)
	}

	// Match project codes so sync works.
	var projCode string
	s.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projCode)
	c.DB().Exec("UPDATE config SET value=? WHERE name='project-code'", projCode)

	// Set nobody capabilities on server.
	s.DB().Exec("UPDATE user SET cap=? WHERE login='nobody'", nobodyCaps)

	t.Cleanup(func() {
		s.Close()
		c.Close()
	})
	return s, c
}

// syncViaHandler runs sync.Sync using a MockTransport backed by HandleSync.
func syncViaHandler(t *testing.T, serverRepo, clientRepo *repo.Repo, opts SyncOpts) *SyncResult {
	t.Helper()
	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, err := HandleSync(context.Background(), serverRepo, req)
		if err != nil {
			t.Fatalf("HandleSync: %v", err)
		}
		return resp
	}}

	var srvCode string
	serverRepo.DB().QueryRow("SELECT value FROM config WHERE name='server-code'").Scan(&srvCode)
	opts.ServerCode = srvCode

	var projCode string
	serverRepo.DB().QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projCode)
	opts.ProjectCode = projCode

	result, err := Sync(context.Background(), clientRepo, transport, opts)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return result
}

func TestSyncPrivateEndToEnd(t *testing.T) {
	// Server has 3 public blobs and 2 private blobs.
	// Client with Private=false gets only public.
	// Client with Private=true gets all.

	t.Run("without_private_flag", func(t *testing.T) {
		serverRepo, clientRepo := newPrivateTestPair(t, "oix")

		// Store 3 public blobs.
		publicUUIDs := make([]string, 3)
		for i := range 3 {
			data := []byte(fmt.Sprintf("public-blob-%d", i))
			_, uuid, err := blob.Store(serverRepo.DB(), data)
			if err != nil {
				t.Fatalf("blob.Store public: %v", err)
			}
			publicUUIDs[i] = uuid
		}

		// Store 2 private blobs.
		privateUUIDs := make([]string, 2)
		for i := range 2 {
			data := []byte(fmt.Sprintf("private-blob-%d", i))
			rid, uuid, err := blob.Store(serverRepo.DB(), data)
			if err != nil {
				t.Fatalf("blob.Store private: %v", err)
			}
			if err := content.MakePrivate(serverRepo.DB(), int64(rid)); err != nil {
				t.Fatalf("MakePrivate: %v", err)
			}
			privateUUIDs[i] = uuid
		}

		// Sync WITHOUT Private flag.
		syncViaHandler(t, serverRepo, clientRepo, SyncOpts{
			Pull:    true,
			Push:    true,
			Private: false,
		})

		// Public blobs should arrive.
		for _, uuid := range publicUUIDs {
			if _, ok := blob.Exists(clientRepo.DB(), uuid); !ok {
				t.Errorf("client missing public blob %s", uuid)
			}
		}

		// Private blobs should NOT arrive.
		for _, uuid := range privateUUIDs {
			if _, ok := blob.Exists(clientRepo.DB(), uuid); ok {
				t.Errorf("client has private blob %s without Private flag", uuid)
			}
		}
	})

	t.Run("with_private_flag", func(t *testing.T) {
		serverRepo, clientRepo := newPrivateTestPair(t, "oix")

		// Store 3 public blobs.
		publicUUIDs := make([]string, 3)
		for i := range 3 {
			data := []byte(fmt.Sprintf("public-blob-priv-%d", i))
			_, uuid, err := blob.Store(serverRepo.DB(), data)
			if err != nil {
				t.Fatalf("blob.Store public: %v", err)
			}
			publicUUIDs[i] = uuid
		}

		// Store 2 private blobs.
		privateUUIDs := make([]string, 2)
		privateRIDs := make([]int64, 2)
		for i := range 2 {
			data := []byte(fmt.Sprintf("private-blob-priv-%d", i))
			rid, uuid, err := blob.Store(serverRepo.DB(), data)
			if err != nil {
				t.Fatalf("blob.Store private: %v", err)
			}
			if err := content.MakePrivate(serverRepo.DB(), int64(rid)); err != nil {
				t.Fatalf("MakePrivate: %v", err)
			}
			privateUUIDs[i] = uuid
			privateRIDs[i] = int64(rid)
		}

		// Sync WITH Private flag.
		syncViaHandler(t, serverRepo, clientRepo, SyncOpts{
			Pull:    true,
			Push:    true,
			Private: true,
		})

		// All public blobs should arrive.
		for _, uuid := range publicUUIDs {
			if _, ok := blob.Exists(clientRepo.DB(), uuid); !ok {
				t.Errorf("client missing public blob %s", uuid)
			}
		}

		// All private blobs should arrive AND be marked private on client.
		for _, uuid := range privateUUIDs {
			rid, ok := blob.Exists(clientRepo.DB(), uuid)
			if !ok {
				t.Errorf("client missing private blob %s", uuid)
				continue
			}
			if !content.IsPrivate(clientRepo.DB(), int64(rid)) {
				t.Errorf("blob %s should be in client's private table", uuid)
			}
		}

		// Public blobs should NOT be in private table.
		for _, uuid := range publicUUIDs {
			rid, ok := blob.Exists(clientRepo.DB(), uuid)
			if !ok {
				continue
			}
			if content.IsPrivate(clientRepo.DB(), int64(rid)) {
				t.Errorf("public blob %s should not be in private table", uuid)
			}
		}
	})

	t.Run("no_x_capability", func(t *testing.T) {
		// Server's nobody lacks 'x' capability — pragma send-private rejected.
		serverRepo, clientRepo := newPrivateTestPair(t, "oi") // no 'x'

		// Store a private blob on server.
		data := []byte("secret-no-cap")
		rid, uuid, err := blob.Store(serverRepo.DB(), data)
		if err != nil {
			t.Fatalf("blob.Store: %v", err)
		}
		content.MakePrivate(serverRepo.DB(), int64(rid))

		// Also store a public blob.
		pubData := []byte("public-no-cap")
		_, pubUUID, err := blob.Store(serverRepo.DB(), pubData)
		if err != nil {
			t.Fatalf("blob.Store public: %v", err)
		}

		// Sync with Private=true, but server denies it.
		syncViaHandler(t, serverRepo, clientRepo, SyncOpts{
			Pull:    true,
			Push:    true,
			Private: true,
		})

		// Public blob should arrive.
		if _, ok := blob.Exists(clientRepo.DB(), pubUUID); !ok {
			t.Error("client missing public blob")
		}

		// Private blob should NOT arrive (server denied send-private).
		if _, ok := blob.Exists(clientRepo.DB(), uuid); ok {
			t.Error("client has private blob despite lacking 'x' capability")
		}
	})
}

func TestSyncPrivateArtifactTransition(t *testing.T) {
	// Server has a private blob. Client syncs with Private=true.
	// Server makes it public. Client syncs again — private status clears.

	serverRepo, clientRepo := newPrivateTestPair(t, "oix")

	// Store a blob and mark private on server.
	data := []byte("transitioning-artifact")
	uuid := hash.SHA1(data)
	rid, _, err := blob.Store(serverRepo.DB(), data)
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	content.MakePrivate(serverRepo.DB(), int64(rid))

	// Phase 1: sync with Private=true — client gets the private blob.
	syncViaHandler(t, serverRepo, clientRepo, SyncOpts{
		Pull:    true,
		Push:    true,
		Private: true,
	})

	clientRID, ok := blob.Exists(clientRepo.DB(), uuid)
	if !ok {
		t.Fatal("client missing blob after first sync")
	}
	if !content.IsPrivate(clientRepo.DB(), int64(clientRID)) {
		t.Fatal("blob should be private on client after first sync")
	}

	// Phase 2: server makes blob public.
	if err := content.MakePublic(serverRepo.DB(), int64(rid)); err != nil {
		t.Fatalf("MakePublic: %v", err)
	}

	// Sync again — the server will emit igot without IsPrivate=true,
	// and the client should clear the private status.
	syncViaHandler(t, serverRepo, clientRepo, SyncOpts{
		Pull:    true,
		Push:    true,
		Private: true,
	})

	if content.IsPrivate(clientRepo.DB(), int64(clientRID)) {
		t.Error("blob should no longer be private on client after transition")
	}
}
