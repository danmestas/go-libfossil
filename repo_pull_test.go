package libfossil_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil"
)

// serverRepo provisions a fossil repo populated with one commit and an
// httptest server hosting its xfer endpoint. Returns the URL to dial.
func serverRepo(t *testing.T) (*libfossil.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "server.fossil")
	repo, err := libfossil.Create(repoPath, libfossil.CreateOpts{})
	if err != nil {
		t.Fatalf("libfossil.Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		resp, err := repo.HandleSync(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return repo, srv.URL
}

func TestRepoPull_Roundtrip(t *testing.T) {
	_, url := serverRepo(t)
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	client, err := libfossil.Create(clientPath, libfossil.CreateOpts{})
	if err != nil {
		t.Fatalf("libfossil.Create client: %v", err)
	}
	defer client.Close()
	res, err := client.Pull(context.Background(), url, libfossil.PullOpts{})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res == nil {
		t.Fatalf("Pull returned nil result")
	}
}

func TestRepoPull_NilCtxPanics(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	client, err := libfossil.Create(clientPath, libfossil.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer client.Close()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil ctx, got none")
		}
	}()
	//nolint:staticcheck // intentionally nil to exercise the assert
	_, _ = client.Pull(nil, "http://x", libfossil.PullOpts{})
}

func TestRepoPull_EmptyURLPanics(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	client, err := libfossil.Create(clientPath, libfossil.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer client.Close()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty url, got none")
		}
	}()
	_, _ = client.Pull(context.Background(), "", libfossil.PullOpts{})
}

func TestRepoPull_TransportError(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	client, err := libfossil.Create(clientPath, libfossil.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer client.Close()
	// Unreachable URL; expect wrapped error, no panic.
	_, err = client.Pull(context.Background(), "http://127.0.0.1:1/missing", libfossil.PullOpts{})
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
}
