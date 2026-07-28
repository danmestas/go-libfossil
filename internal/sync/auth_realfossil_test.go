package sync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// Issue #203: authentication against a canonical fossil server never worked.
// Every credentialed sync was answered with "login failed" whether or not the
// password was right, so push — which always requires a login — was unusable
// against any real server.
//
// Nothing in the suite authenticated against real fossil: clone was only ever
// exercised against servers granting anonymous read, where the login card is
// never sent, so the whole authenticated path was covered by self-consistency
// alone. These tests close that gap: they stand up a canonical fossil server
// with anonymous access removed, so a login card is the only way in.

// requireLoginOnly turns repoPath into a repository that genuinely requires
// authentication: user gets password and caps, and every other account —
// notably "anonymous" and "nobody", which fossil's check_login lets through
// unconditionally — is stripped of capabilities. Without the strip a server
// answers anonymous requests happily and the login card is never exercised.
func requireLoginOnly(t *testing.T, bin, repoPath, user, password, caps string) {
	t.Helper()

	run := func(args ...string) {
		t.Helper()
		full := append(args, "-R", repoPath)
		if out, err := exec.Command(bin, full...).CombinedOutput(); err != nil {
			t.Fatalf("fossil %v: %v\n%s", full, err, out)
		}
	}

	run("user", "new", user, "sync user", password)
	run("user", "password", user, password)
	run("user", "capabilities", user, caps)

	out, err := exec.Command(bin, "sql", "-R", repoPath, "SELECT login FROM user").Output()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		login := strings.Trim(strings.TrimSpace(line), "'\"")
		if login == "" || login == user {
			continue
		}
		run("user", "capabilities", login, "")
	}
}

// fossilBlobExists reports whether repoPath holds content for uuid. A phantom
// (size<0) is a placeholder, not received content, so it does not count.
func fossilBlobExists(t *testing.T, bin, repoPath, uuid string) bool {
	t.Helper()
	out, err := exec.Command(bin, "sql", "-R", repoPath,
		"SELECT count(*) FROM blob WHERE uuid='"+uuid+"' AND size>=0").Output()
	if err != nil {
		t.Fatalf("count blob %s: %v", uuid, err)
	}
	return strings.TrimSpace(string(out)) == "1"
}

// TestSharedSecretMatchesFossilStoredHash checks our password derivation
// against what the fossil binary actually writes into user.pw, rather than
// against a constant transcribed from its source. A login signature is
// computed over exactly that stored value, so if this diverges every
// authenticated sync is refused — with the same "login failed" a wrong
// password gets, which is what made #203 so hard to read.
func TestSharedSecretMatchesFossilStoredHash(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()

	repoPath := filepath.Join(dir, "secret.fossil")
	if out, err := exec.Command(bin, "new", repoPath).CombinedOutput(); err != nil {
		t.Fatalf("fossil new: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"user", "new", "syncer", "sync user", "pw123"},
		{"user", "password", "syncer", "pw123"},
	} {
		if out, err := exec.Command(bin, append(args, "-R", repoPath)...).CombinedOutput(); err != nil {
			t.Fatalf("fossil %v: %v\n%s", args, err, out)
		}
	}

	fossilSQL := func(query string) string {
		t.Helper()
		out, err := exec.Command(bin, "sql", "-R", repoPath, query).Output()
		if err != nil {
			t.Fatalf("fossil sql %q: %v", query, err)
		}
		return strings.Trim(strings.TrimSpace(string(out)), "'\"")
	}
	projectCode := fossilSQL("SELECT value FROM config WHERE name='project-code'")
	stored := fossilSQL("SELECT pw FROM user WHERE login='syncer'")
	if len(stored) != 40 {
		t.Fatalf("fixture bug: fossil stored a %d-character password, want a 40-character hash", len(stored))
	}

	if got := sharedSecret("pw123", "syncer", projectCode); got != stored {
		t.Fatalf("derived shared secret %q, but fossil stores %q for the same "+
			"project code, login and password", got, stored)
	}
}

// TestRealFossilCloneRequiresCredentials is the #203 pull-side regression: a
// canonical fossil server that grants nobody anonymous access must accept our
// login card with the right password, and must reject it with the wrong one.
//
// The bug made both outcomes identical, so this asserts the two paths diverge:
// correct credentials transfer content, a wrong password is refused, and the
// refusal names the login failure — proving the server actually evaluated the
// password rather than rejecting a malformed card before comparing it.
func TestRealFossilCloneRequiresCredentials(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()

	remotePath := filepath.Join(dir, "remote.fossil")
	if out, err := exec.Command(bin, "new", remotePath).CombinedOutput(); err != nil {
		t.Fatalf("fossil new: %v\n%s", err, out)
	}

	workDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}
	runInWork := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fossil %v: %v\n%s", args, err, out)
		}
	}
	runInWork("open", remotePath)
	if err := os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("content behind a login\n"), 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	runInWork("add", "hello.txt")
	runInWork("commit", "-m", "content behind a login", "--no-warnings")
	runInWork("close")

	// "g" is clone, "o" read, "i" write — the capability set a sync account needs.
	requireLoginOnly(t, bin, remotePath, "syncer", "pw123", "goi")

	serverURL := startFossilServer(t, remotePath)

	clone := func(name string, opts CloneOpts) (*CloneResult, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, res, err := Clone(ctx, filepath.Join(dir, name), &HTTPTransport{URL: serverURL}, opts)
		if r != nil {
			r.Close()
		}
		return res, err
	}

	// The fixture is only meaningful if the server truly refuses anonymous
	// access — otherwise a "successful" credentialed clone proves nothing.
	if _, err := clone("anon.fossil", CloneOpts{}); err == nil {
		t.Fatal("anonymous clone succeeded; the fixture does not require authentication")
	}

	_, wrongErr := clone("wrong.fossil", CloneOpts{User: "syncer", Password: "not-the-password"})
	if wrongErr == nil {
		t.Fatal("clone with a wrong password succeeded; credentials are not being checked")
	}
	if !strings.Contains(wrongErr.Error(), "login failed") {
		t.Fatalf("wrong-password clone failed with %v, want the server's \"login failed\" — "+
			"a different rejection means the login card never reached password comparison", wrongErr)
	}

	res, err := clone("good.fossil", CloneOpts{User: "syncer", Password: "pw123"})
	if err != nil {
		t.Fatalf("clone with correct credentials failed: %v", err)
	}
	if res.BlobsRecvd == 0 {
		t.Fatal("authenticated clone received 0 blobs; it authenticated but transferred nothing")
	}
}

// TestRealFossilPushRequiresCredentials is the #203 push-side regression.
// Push always needs a login, so this is the operation the bug made unusable.
// It asserts a local commit actually lands in the canonical server's blob
// table under correct credentials, and does not under a wrong password.
func TestRealFossilPushRequiresCredentials(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()

	// A repo of ours, cloned by canonical fossil so both ends share a project
	// code — the value the login signature is derived from.
	localPath := filepath.Join(dir, "local.fossil")
	r, err := repo.Create(localPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	if _, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "base.txt", Content: []byte("base revision\n")}},
		Comment: "base",
		User:    "testuser",
		Time:    time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Checkin base: %v", err)
	}
	r.Close()

	remotePath := filepath.Join(dir, "remote.fossil")
	if out, err := exec.Command(bin, "clone", localPath, remotePath).CombinedOutput(); err != nil {
		t.Fatalf("fossil clone: %v\n%s", err, out)
	}
	requireLoginOnly(t, bin, remotePath, "syncer", "pw123", "goi")

	// The commit that must cross the wire — created after the remote was
	// cloned, so the server has no way to already hold it.
	r2, err := repo.Open(localPath)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r2.Close()
	_, pushedUUID, err := manifest.Checkin(r2, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "pushed.txt", Content: []byte("pushed under authentication\n")}},
		Comment: "pushed under authentication",
		User:    "testuser",
		Time:    time.Date(2026, 3, 15, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin pushed: %v", err)
	}

	serverURL := startFossilServer(t, remotePath)

	push := func(password string) (*SyncResult, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return Sync(ctx, r2, &HTTPTransport{URL: serverURL}, SyncOpts{
			Push:     true,
			User:     "syncer",
			Password: password,
		})
	}

	wrongRes, wrongErr := push("not-the-password")
	if wrongErr == nil && len(wrongRes.Errors) == 0 {
		t.Fatal("push with a wrong password reported no error; credentials are not being checked")
	}
	if fossilBlobExists(t, bin, remotePath, pushedUUID) {
		t.Fatalf("artifact %s reached the server under a wrong password", pushedUUID)
	}

	res, err := push("pw123")
	if err != nil {
		t.Fatalf("authenticated push failed: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("authenticated push reported errors: %v", res.Errors)
	}
	if !fossilBlobExists(t, bin, remotePath, pushedUUID) {
		t.Fatalf("authenticated push reported success but artifact %s is not in the server's blob table; "+
			"the login was accepted without any content moving", pushedUUID)
	}

	// Bytes in the blob table are not yet a check-in. The server crosslinks
	// what it receives, so the commit must also show up on its timeline —
	// that is the difference between content arriving and content landing.
	timeline, err := exec.Command(bin, "timeline", "-n", "20", "-R", remotePath).Output()
	if err != nil {
		t.Fatalf("fossil timeline: %v", err)
	}
	if !strings.Contains(string(timeline), "pushed under authentication") {
		t.Fatalf("pushed check-in is not on the server's timeline:\n%s", timeline)
	}
}
