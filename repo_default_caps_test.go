package libfossil

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// A new repository's built-in users must carry exactly the capabilities
// `fossil new` gives them (src/db.c db_create_default_users). nobody's set is
// what every unauthenticated request runs with, so anything beyond fossil's
// "gjorz" hands strangers push, wiki and ticket writes, or Setup.
func TestCreateSeedsFossilDefaultUsers(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "repo.fossil"), CreateOpts{User: "admin"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()

	users, err := r.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	got := make(map[string]string, len(users))
	for _, u := range users {
		got[u.Login] = u.Caps
	}
	want := map[string]string{
		"admin":     "s",
		"anonymous": "hz",
		"nobody":    "gjorz",
		"developer": "ei",
		"reader":    "kptw",
	}
	if len(got) != len(want) {
		t.Fatalf("users = %v, want %v", got, want)
	}
	for login, caps := range want {
		if got[login] != caps {
			t.Errorf("user %q caps = %q, want %q", login, got[login], caps)
		}
	}
}

// serveFrom is a transport that hands each request straight to server's
// sync handler, so a test exercises the same path an HTTP peer reaches.
func serveFrom(server *Repo) Transport {
	return TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		return server.HandleSync(ctx, payload)
	})
}

// newServedPair creates a fresh repository owned by "admin" and clones it
// anonymously, which fossil's default nobody capabilities permit.
func newServedPair(t *testing.T) (server, client *Repo) {
	t.Helper()
	dir := t.TempDir()
	server, err := Create(filepath.Join(dir, "server.fossil"), CreateOpts{User: "admin"})
	if err != nil {
		t.Fatalf("Create server: %v", err)
	}
	t.Cleanup(func() { server.Close() })

	if _, _, err := server.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "a.txt", Content: []byte("server\n")}},
		Comment: "initial",
		User:    "admin",
		Time:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("server Commit: %v", err)
	}

	client, _, err = Clone(context.Background(), filepath.Join(dir, "client.fossil"),
		serveFrom(server), CloneOpts{})
	if err != nil {
		t.Fatalf("anonymous Clone of a fresh repository: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return server, client
}

func hasArtifact(t *testing.T, r *Repo, uuid string) bool {
	t.Helper()
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob WHERE uuid=?", uuid).Scan(&n); err != nil {
		t.Fatalf("query blob: %v", err)
	}
	return n > 0
}

// An unauthenticated client may clone and pull a fresh repository but must not
// push to it: fossil's nobody has g and o, not i.
func TestFreshRepoRefusesAnonymousPush(t *testing.T) {
	server, client := newServedPair(t)
	ctx := context.Background()

	// Pull first: a pull-only sync from a client holding unsent check-ins
	// does not settle, which is unrelated to capabilities.
	_, pulled, err := server.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "c.txt", Content: []byte("later\n")}},
		Comment: "after the clone",
		User:    "admin",
		Time:    time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("server Commit: %v", err)
	}
	if _, err := client.Sync(ctx, serveFrom(server), SyncOpts{Pull: true}); err != nil {
		t.Fatalf("anonymous pull: %v", err)
	}
	if !hasArtifact(t, client, pulled) {
		t.Fatal("anonymous pull did not deliver the server's new check-in")
	}

	_, pushed, err := client.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "b.txt", Content: []byte("client\n")}},
		Comment: "from an anonymous client",
		User:    "anon",
		Time:    time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("client Commit: %v", err)
	}
	// The outcome under test is the server's state, not the client's error.
	_, _ = client.Sync(ctx, serveFrom(server), SyncOpts{Push: true})
	if hasArtifact(t, server, pushed) {
		t.Fatal("server stored a check-in pushed without credentials")
	}
}

// The user that created the repository holds Setup, which in fossil includes
// check-in rights, so an authenticated push by that user must be accepted.
func TestFreshRepoAcceptsCreatorPush(t *testing.T) {
	server, client := newServedPair(t)
	if err := server.SetPassword("admin", "secret"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	_, pushed, err := client.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "b.txt", Content: []byte("admin\n")}},
		Comment: "from the creator",
		User:    "admin",
		Time:    time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("client Commit: %v", err)
	}
	res, err := client.Sync(context.Background(), serveFrom(server), SyncOpts{
		Push:     true,
		User:     "admin",
		Password: "secret",
	})
	if err != nil {
		t.Fatalf("authenticated push: %v", err)
	}
	if !hasArtifact(t, server, pushed) {
		t.Fatalf("server did not store the creator's push (errors: %v)", res.Errors)
	}
}
