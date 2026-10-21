package sync_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/testutil"
)

// openRepoDB opens a repository fossil wrote, for reading its rcvfrom rows.
func openRepoDB(t *testing.T, path string) *db.DB {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// startFossilHTTP serves path with the fossil binary and returns its URL with
// user's credentials, on 127.0.0.1 so both clients record the same address.
func startFossilHTTP(t *testing.T, bin, path, user, password string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	testutil.StartServer(t, bin, "server", fmt.Sprintf("--port=%d", port), path)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fossil server not ready on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return withUser("http://"+addr, user, password)
}

func withUser(base, user, password string) string {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// fossilCommitFile commits name with text in a checkout of repoPath at work,
// as alice.
func fossilCommitFile(t *testing.T, bin, work, repoPath, name, text string) {
	t.Helper()
	if _, err := os.Stat(work); os.IsNotExist(err) {
		if err := os.Mkdir(work, 0o755); err != nil {
			t.Fatal(err)
		}
		fossilOK(t, bin, work, "settings", "autosync", "off", "-R", repoPath)
		fossilOK(t, bin, work, "open", "--nosync", repoPath)
	}
	if err := os.WriteFile(filepath.Join(work, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	fossilOK(t, bin, work, "add", name)
	fossilOK(t, bin, work, "commit", "-m", "add "+name, "--no-warnings", "--user", "alice")
}

// fossilIntegrity runs fossil's own checks on a repository go-libfossil
// holds open, after moving its WAL into the file fossil sizes up.
func fossilIntegrity(t *testing.T, bin, dir string, r *repo.Repo) {
	t.Helper()
	if err := r.Checkpoint(db.CheckpointTruncate); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	path := r.Path()
	out, err := fossilRun(bin, dir, "test-integrity", "-R", path)
	if err != nil || !strings.Contains(out, " 0 errors") {
		t.Fatalf("fossil test-integrity %s: %v\n%s", path, err, out)
	}
	fossilOK(t, bin, dir, "rebuild", path)
}

// sameShape reports how fossil's receipt row f and go-libfossil's g differ in
// what both should agree on: the user, the address, the blobs, a Julian-day
// mtime, and whether there is a nonce and that it is a SHA-1.
func sameShape(f, g rcvRow) bool {
	nonceShape := func(n string) string {
		if n == "NULL" {
			return n
		}
		if sha1Hex.MatchString(n) {
			return "sha1"
		}
		return "other:" + n
	}
	return f.Login == g.Login && f.IPAddr == g.IPAddr && f.Blobs == g.Blobs &&
		f.Julian && g.Julian && nonceShape(f.Nonce) == nonceShape(g.Nonce)
}

func compareReceipts(t *testing.T, what string, fossil, glf []rcvRow) {
	t.Helper()
	t.Logf("%s: fossil %+v, go-libfossil %+v", what, fossil, glf)
	if len(fossil) != len(glf) {
		t.Fatalf("%s: fossil recorded %+v, go-libfossil %+v", what, fossil, glf)
	}
	for i := range fossil {
		if !sameShape(fossil[i], glf[i]) {
			t.Errorf("%s row %d: fossil %+v, go-libfossil %+v", what, i, fossil[i], glf[i])
		}
	}
}

// A clone and a pull from a fossil server record the receipts fossil's own
// client records for them, and a push into a go-libfossil server records the
// receipt a fossil server records for the same push. Nonces and times differ
// by nature; their shape must not.
func TestReceiptsMatchRealFossil(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	origin := filepath.Join(dir, "origin.fossil")
	fossilOK(t, bin, dir, "new", "--admin-user", "alice", origin)
	fossilOK(t, bin, dir, "user", "password", "alice", "pw", "-R", origin)
	fossilCommitFile(t, bin, filepath.Join(dir, "w-origin"), origin, "one.txt", "one\n")
	originURL := startFossilHTTP(t, bin, origin, "alice", "pw")

	// Clone: fossil's client against go-libfossil's.
	fClone := filepath.Join(dir, "f-clone.fossil")
	fossilOK(t, bin, dir, "clone", originURL, fClone)
	gClone, _, err := sync.Clone(ctx, filepath.Join(dir, "g-clone.fossil"),
		&sync.HTTPTransport{URL: originURL}, sync.CloneOpts{User: "alice", Password: "pw"})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer gClone.Close()
	compareReceipts(t, "clone", rcvRows(t, openRepoDB(t, fClone)), rcvRows(t, gClone.DB()))

	// Pull a new check-in.
	fossilCommitFile(t, bin, filepath.Join(dir, "w-origin"), origin, "two.txt", "two\n")
	fossilOK(t, bin, dir, "pull", originURL, "-R", fClone)
	if _, err := sync.Sync(ctx, gClone, &sync.HTTPTransport{URL: originURL},
		sync.SyncOpts{Pull: true, User: "alice", Password: "pw"}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	compareReceipts(t, "pull", rcvRows(t, openRepoDB(t, fClone)), rcvRows(t, gClone.DB()))
	fossilIntegrity(t, bin, dir, gClone)

	// Push: the same fossil commit into the fossil server and into the
	// go-libfossil clone, served, which holds what the fossil server holds.
	pc, err := gClone.Config("project-code")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SetPassword(gClone.DB(), pc, "alice", "pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	gURL := withUser(serveRepo(ctx, t, gClone), "alice", "pw")
	work := filepath.Join(dir, "w-clone")
	fossilCommitFile(t, bin, work, fClone, "three.txt", "three\n")
	fossilOK(t, bin, work, "push", originURL)
	fossilOK(t, bin, work, "push", gURL)
	fRows := rcvRows(t, openRepoDB(t, origin))
	gRows := rcvRows(t, gClone.DB())
	compareReceipts(t, "push", fRows[len(fRows)-1:], gRows[len(gRows)-1:])
	fossilIntegrity(t, bin, dir, gClone)
}
