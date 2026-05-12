package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	"github.com/danmestas/libfossil/internal/uv"
	"github.com/danmestas/libfossil/internal/xfer"
)

// newTestRepoPair creates a server and client repo, both with UV schema.
func newTestRepoPair(t *testing.T) (server, client *repo.Repo) {
	t.Helper()
	dir := t.TempDir()

	sPath := filepath.Join(dir, "server.fossil")
	s, err := repo.Create(sPath, "test", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("server repo: %v", err)
	}
	uv.EnsureSchema(s.DB())

	cPath := filepath.Join(dir, "client.fossil")
	c, err := repo.Create(cPath, "test", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("client repo: %v", err)
	}
	uv.EnsureSchema(c.DB())

	t.Cleanup(func() {
		s.Close()
		c.Close()
	})
	return s, c
}

func TestSyncUV_PullFromServer(t *testing.T) {
	serverRepo, clientRepo := newTestRepoPair(t)

	// Server has a UV file.
	uv.Write(serverRepo.DB(), "wiki/page.txt", []byte("hello wiki"), 100)

	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, _ := HandleSync(context.Background(), serverRepo, req)
		return resp
	}}
	_, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true, Push: true, UV: true,
		ProjectCode: "p", ServerCode: "s",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Client should now have the file.
	content, mtime, hash, err := uv.Read(clientRepo.DB(), "wiki/page.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(content) != "hello wiki" || mtime != 100 || hash == "" {
		t.Errorf("got content=%q mtime=%d hash=%q", content, mtime, hash)
	}
}

func TestSyncUV_PushToServer(t *testing.T) {
	serverRepo, clientRepo := newTestRepoPair(t)

	// Client has a UV file.
	uv.Write(clientRepo.DB(), "data/config.json", []byte(`{"key":"val"}`), 200)

	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, _ := HandleSync(context.Background(), serverRepo, req)
		return resp
	}}
	_, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true, Push: true, UV: true,
		ProjectCode: "p", ServerCode: "s",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Server should now have the file.
	content, _, _, err := uv.Read(serverRepo.DB(), "data/config.json")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(content) != `{"key":"val"}` {
		t.Errorf("server content = %q", content)
	}
}

func TestSyncUV_Bidirectional(t *testing.T) {
	serverRepo, clientRepo := newTestRepoPair(t)

	uv.Write(serverRepo.DB(), "server-file.txt", []byte("from server"), 100)
	uv.Write(clientRepo.DB(), "client-file.txt", []byte("from client"), 200)

	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, _ := HandleSync(context.Background(), serverRepo, req)
		return resp
	}}
	_, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true, Push: true, UV: true,
		ProjectCode: "p", ServerCode: "s",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Both should have both files.
	sc, _, _, _ := uv.Read(serverRepo.DB(), "client-file.txt")
	cc, _, _, _ := uv.Read(clientRepo.DB(), "server-file.txt")
	if string(sc) != "from client" {
		t.Errorf("server missing client file: %q", sc)
	}
	if string(cc) != "from server" {
		t.Errorf("client missing server file: %q", cc)
	}
}

func TestSyncUV_Deletion(t *testing.T) {
	serverRepo, clientRepo := newTestRepoPair(t)

	// Both have the file initially.
	uv.Write(serverRepo.DB(), "old.txt", []byte("data"), 100)
	uv.Write(clientRepo.DB(), "old.txt", []byte("data"), 100)

	// Server deletes it (newer mtime).
	uv.Delete(serverRepo.DB(), "old.txt", 200)

	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, _ := HandleSync(context.Background(), serverRepo, req)
		return resp
	}}
	_, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true, Push: true, UV: true,
		ProjectCode: "p", ServerCode: "s",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Client should have tombstone.
	_, mtime, hash, _ := uv.Read(clientRepo.DB(), "old.txt")
	if hash != "" {
		t.Errorf("expected tombstone, got hash=%q", hash)
	}
	if mtime != 200 {
		t.Errorf("mtime = %d, want 200", mtime)
	}
}

func TestSyncUV_NoUVFlag_SkipsUV(t *testing.T) {
	serverRepo, clientRepo := newTestRepoPair(t)
	uv.Write(serverRepo.DB(), "test.txt", []byte("data"), 100)

	transport := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		resp, _ := HandleSync(context.Background(), serverRepo, req)
		return resp
	}}
	_, err := Sync(context.Background(), clientRepo, transport, SyncOpts{
		Pull: true, Push: true, UV: false, // UV disabled
		ProjectCode: "p", ServerCode: "s",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Client should NOT have the UV file.
	content, _, _, _ := uv.Read(clientRepo.DB(), "test.txt")
	if content != nil {
		t.Error("UV file should not sync when UV=false")
	}
}
