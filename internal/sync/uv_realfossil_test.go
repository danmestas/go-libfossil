package sync_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/uv"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// TestRealFossilUVSyncPushesWithoutPushCard checks that `fossil uv sync`,
// which sends uvfile cards with no push card, stores its file on the server
// for a user who may write unversioned files (§7.3).
func TestRealFossilUVSyncPushesWithoutPushCard(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()

	srv, err := repo.Create(filepath.Join(dir, "server.fossil"), "testuser",
		simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := serveRepo(ctx, t, srv)

	clonePath := filepath.Join(dir, "clone.fossil")
	fossilOK(t, bin, dir, "clone", base, clonePath)
	var pc string
	if err := srv.DB().QueryRow(
		"SELECT value FROM config WHERE name='project-code'").Scan(&pc); err != nil {
		t.Fatalf("project-code: %v", err)
	}
	if err := auth.CreateUser(srv.DB(), pc, "alice", "pw123", "oiy"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", base, err)
	}
	u.User = url.UserPassword("alice", "pw123")

	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("unversioned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fossilOK(t, bin, dir, "uv", "add", file, "--as", "f.txt", "-R", clonePath)
	fossilOK(t, bin, dir, "uv", "sync", u.String(), "-R", clonePath)

	content, _, _, err := uv.Read(srv.DB(), "f.txt")
	if err != nil {
		t.Fatalf("uv.Read: %v", err)
	}
	if string(content) != "unversioned\n" {
		t.Fatalf("server holds f.txt as %q after fossil uv sync, want %q",
			content, "unversioned\n")
	}
}
