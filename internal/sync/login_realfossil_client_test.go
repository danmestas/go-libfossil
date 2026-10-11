package sync_test

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/auth"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// TestRealFossilClientLogsIntoServer checks the server's login handling
// against the real fossil client. The server checks that each login card's
// nonce is the SHA-1 of the body after the login line, so it must hash
// exactly the bytes fossil's http_build_login_card hashed: a pull and a push
// under the right password succeed, and the wrong password is refused with
// fossil's own "login failed".
func TestRealFossilClientLogsIntoServer(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()

	srv, err := repo.Create(filepath.Join(dir, "server.fossil"), "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srv.Close()
	if _, _, err := manifest.Checkin(srv, manifest.CheckinOpts{
		Comment: "seed", User: "testuser", Parent: libfossil.FslID(0),
		Files: []manifest.File{{Name: "seed.txt", Content: []byte("seed\n")}},
	}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := serveRepo(ctx, t, srv)

	// Clone while nobody may still clone, then lock nobody out: from here on
	// a login card is the only way in.
	clonePath := filepath.Join(dir, "clone.fossil")
	fossilOK(t, bin, dir, "clone", base, clonePath)
	var pc string
	if err := srv.DB().QueryRow(
		"SELECT value FROM config WHERE name='project-code'").Scan(&pc); err != nil {
		t.Fatalf("project-code: %v", err)
	}
	if err := auth.CreateUser(srv.DB(), pc, "alice", "pw123", "oi"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := srv.DB().Exec("UPDATE user SET cap='' WHERE login='nobody'"); err != nil {
		t.Fatalf("lock out nobody: %v", err)
	}

	withCreds := func(password string) string {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatalf("parse %s: %v", base, err)
		}
		u.User = url.UserPassword("alice", password)
		return u.String()
	}

	// fossil reports a server's error card on its output and still exits 0.
	out, _ := fossilRun(bin, dir, "pull", withCreds("wrong"), "-R", clonePath, "--once")
	if !strings.Contains(out, "login failed") {
		t.Fatalf("pull with the wrong password was not refused with \"login failed\":\n%s", out)
	}
	fossilOK(t, bin, dir, "pull", withCreds("pw123"), "-R", clonePath, "--once")

	commitInClone(t, bin, filepath.Join(dir, "work"), clonePath, "alice's change")
	fossilOK(t, bin, filepath.Join(dir, "work"), "push", withCreds("pw123"), "--once")

	if _, err := manifest.Crosslink(srv); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	var n int
	if err := srv.DB().QueryRow(
		"SELECT count(*) FROM event WHERE type='ci' AND comment='alice''s change'").Scan(&n); err != nil {
		t.Fatalf("count check-ins: %v", err)
	}
	if n != 1 {
		t.Fatalf("server holds %d of alice's check-ins after an authenticated push, want 1", n)
	}
}

// commitInClone opens clonePath in work without syncing and commits one new
// file there with the given comment.
func commitInClone(t *testing.T, bin, work, clonePath, comment string) {
	t.Helper()
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilOK(t, bin, work, "settings", "autosync", "off", "-R", clonePath)
	fossilOK(t, bin, work, "open", "--nosync", clonePath)
	file := filepath.Join(work, "alice.txt")
	if err := os.WriteFile(file, []byte("from alice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fossilOK(t, bin, work, "add", "alice.txt")
	fossilOK(t, bin, work, "commit", "-m", comment, "--no-warnings")
}

func fossilRun(bin, dir string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FOSSIL_HOME="+dir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// fossilOK runs fossil and fails the test on a non-zero exit or on an error
// card the server sent, which fossil prints as "Error:" without failing.
func fossilOK(t *testing.T, bin, dir string, args ...string) {
	t.Helper()
	out, err := fossilRun(bin, dir, args...)
	if err != nil || strings.Contains(out, "Error:") {
		t.Fatalf("fossil %v: %v\n%s", args, err, out)
	}
}
