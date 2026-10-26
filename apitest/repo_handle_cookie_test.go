package apitest_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// TestHandleSyncWithOptsLoginCookie pins HandleOpts.LoginCookie: an
// embedder's own HTTP handler that forwards the x-f-l-c cookie lets a client
// that moved its login there after the first round's server-version (§6.5)
// push as itself, where the round would otherwise arrive as nobody.
func TestHandleSyncWithOptsLoginCookie(t *testing.T) {
	const pc = "77dc1a9065e8b8ff0d35abe4e26498cdf9576d6a"
	dir := t.TempDir()
	server, err := libfossil.Create(filepath.Join(dir, "server.fossil"),
		libfossil.CreateOpts{User: "admin", ProjectCode: pc})
	if err != nil {
		t.Fatalf("Create server: %v", err)
	}
	defer server.Close()
	alice := libfossil.UserOpts{Login: "alice", Password: "pw", Caps: "oi"}
	if err := server.CreateUser(alice); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := server.SetCaps("nobody", ""); err != nil {
		t.Fatalf("SetCaps: %v", err)
	}

	var cookieRounds atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var opts libfossil.HandleOpts
		if c, err := r.Cookie("x-f-l-c"); err == nil {
			opts.LoginCookie = c.Value
			cookieRounds.Add(1)
		}
		resp, err := server.HandleSyncWithOpts(r.Context(), body, opts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-fossil")
		_, _ = w.Write(resp)
	}))
	defer srv.Close()

	client, err := libfossil.Create(filepath.Join(dir, "client.fossil"),
		libfossil.CreateOpts{User: "alice", ProjectCode: pc})
	if err != nil {
		t.Fatalf("Create client: %v", err)
	}
	defer client.Close()
	_, uuid, err := client.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: "a.txt", Content: []byte("pushed as alice\n")},
		},
		Comment: "alice's change", User: "alice",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, err := client.Sync(context.Background(), libfossil.NewHTTPTransport(srv.URL),
		libfossil.SyncOpts{Push: true, User: "alice", Password: "pw"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("sync errors: %v", strings.Join(res.Errors, "; "))
	}
	if cookieRounds.Load() == 0 {
		t.Fatal("no round logged in through the cookie")
	}
	if _, err := server.ResolveVersion(uuid); err != nil {
		t.Fatalf("server lacks check-in %s after the push: %v", uuid, err)
	}
}
