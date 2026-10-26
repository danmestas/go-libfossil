package libfossil

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/testutil"
)

func TestCreateAndOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Path() != path {
		t.Errorf("Path() = %q, want %q", r.Path(), path)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatal("repo file should exist after Create")
	}

	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if r2.Path() != path {
		t.Errorf("Path() = %q, want %q", r2.Path(), path)
	}
}

func TestCreateAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	_, err = Create(path, CreateOpts{User: "test"})
	if err == nil {
		t.Error("Create on existing repo should fail")
	}
}

func TestOpenNotFound(t *testing.T) {
	_, err := Open("/nonexistent/path.fossil")
	if err == nil {
		t.Error("Open on nonexistent path should fail")
	}
}

func TestCreate_ProjectCode_Explicit(t *testing.T) {
	const code = "0123456789abcdef0123456789abcdef01234567"
	path := filepath.Join(t.TempDir(), "test.fossil")

	r, err := Create(path, CreateOpts{User: "test", ProjectCode: code})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r2.Close()
	got, err := r2.inner.Config("project-code")
	if err != nil {
		t.Fatalf("Config(project-code): %v", err)
	}
	if got != code {
		t.Errorf("project-code = %q, want %q", got, code)
	}
}

func TestCreate_ProjectCode_Invalid(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"too short", "deadbeef"},
		{"uppercase", strings.Repeat("DEADBEEF", 5)},
		{"non-hex char", strings.Repeat("g", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.fossil")
			_, err := Create(path, CreateOpts{User: "test", ProjectCode: tc.code})
			if err == nil {
				t.Fatalf("Create with ProjectCode=%q should fail", tc.code)
			}
			if !strings.Contains(err.Error(), "invalid CreateOpts.ProjectCode") {
				t.Errorf("error %q missing 'invalid CreateOpts.ProjectCode'", err)
			}
			if _, statErr := os.Stat(path); statErr == nil {
				t.Error("repo file should not exist after rejected Create")
			}
		})
	}
}

// TestCreate_ProjectCode_FossilValidation creates a Go-side repo with an
// explicit ProjectCode, then runs vanilla `fossil rebuild` against it and
// reads the project-code back via `fossil sql` to confirm vanilla Fossil
// accepts the value and stores it as-is.
func TestCreate_ProjectCode_FossilValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fossil validation")
	}
	fossilBin := testutil.RequireFossilBin(t)
	const code = "0123456789abcdef0123456789abcdef01234567"
	path := filepath.Join(t.TempDir(), "test.fossil")

	r, err := Create(path, CreateOpts{User: "test", ProjectCode: code})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, err := exec.Command(fossilBin, "rebuild", path).CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}

	out, err = exec.Command(fossilBin, "sql", "-R", path,
		"SELECT value FROM config WHERE name='project-code';").Output()
	if err != nil {
		t.Fatalf("fossil sql: %v", err)
	}
	got := string(bytes.Trim(out, "'\n \t"))
	if got != code {
		t.Errorf("project-code via fossil sql = %q, want %q", got, code)
	}
}

func TestCreate_ProjectCode_EmptyGeneratesHex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()

	code, err := r.inner.Config("project-code")
	if err != nil {
		t.Fatalf("Config(project-code): %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(code) {
		t.Errorf("generated project-code %q does not match ^[0-9a-f]{40}$", code)
	}
}

// Create refuses a path whose parent the filesystem confirms is missing or
// is not a directory, and proceeds when the filesystem cannot say, as under
// TinyGo wasm, where a repository lives in a SQLite VFS and os.Stat is
// unsupported (#509).
func TestCreateParentDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(dir, "no-such-dir", "r.fossil"): "parent directory does not exist",
		filepath.Join(file, "r.fossil"):               "parent is not a directory",
	} {
		_, err := Create(path, CreateOpts{User: "test"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Create(%s) = %v, want %q", path, err, want)
		}
	}

	statUnsupported := func(string) (fs.FileInfo, error) { return nil, errors.ErrUnsupported }
	saved := statParent
	statParent = statUnsupported
	t.Cleanup(func() { statParent = saved })
	r, err := Create(filepath.Join(dir, "r.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create with an unsupported stat: %v, want it to proceed", err)
	}
	r.Close()
}

func adminLogin(t *testing.T, r *Repo) string {
	t.Helper()
	var login string
	if err := r.DB().QueryRow("SELECT login FROM user WHERE uid=1").Scan(&login); err != nil {
		t.Fatalf("read seeded user: %v", err)
	}
	return login
}

// Create with no CreateOpts.User and no user in the environment must still
// create a repository, seeding fossil's fallback login "root", not panic on
// an empty login.
func TestCreate_NoUserAnywhereSeedsRoot(t *testing.T) {
	for _, name := range []string{"FOSSIL_USER", "USER", "LOGNAME", "USERNAME"} {
		t.Setenv(name, "")
	}
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()
	if got := adminLogin(t, r); got != "root" {
		t.Errorf("seeded login = %q, want root", got)
	}
}

// The environment is consulted in fossil's order: FOSSIL_USER, then USER,
// then LOGNAME, then USERNAME.
func TestCreate_DefaultLoginEnvOrder(t *testing.T) {
	t.Setenv("FOSSIL_USER", "")
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "from-logname")
	t.Setenv("USERNAME", "from-username")
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()
	if got := adminLogin(t, r); got != "from-logname" {
		t.Errorf("seeded login = %q, want LOGNAME before USERNAME", got)
	}

	t.Setenv("USER", "from-user")
	r2, err := Create(filepath.Join(t.TempDir(), "r2.fossil"), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r2.Close()
	if got := adminLogin(t, r2); got != "from-user" {
		t.Errorf("seeded login = %q, want USER before LOGNAME", got)
	}

	t.Setenv("FOSSIL_USER", "from-fossil-user")
	r4, err := Create(filepath.Join(t.TempDir(), "r4.fossil"), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r4.Close()
	if got := adminLogin(t, r4); got != "from-fossil-user" {
		t.Errorf("seeded login = %q, want FOSSIL_USER first", got)
	}

	// An explicit user always wins.
	r3, err := Create(filepath.Join(t.TempDir(), "r3.fossil"), CreateOpts{User: "explicit"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r3.Close()
	if got := adminLogin(t, r3); got != "explicit" {
		t.Errorf("seeded login = %q, want the explicit user", got)
	}
}

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
