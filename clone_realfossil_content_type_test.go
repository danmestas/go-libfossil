package libfossil_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/testutil"
)

// TestCloneFromRealFossilUncompressedReply is the end-to-end regression for
// issue #164: cloning through the public HTTP Transport against a real fossil
// server must succeed on the very first exchange round, even though that
// server's clone reply uses the §4 uncompressed/plain-card framing rather than
// the §4.1 compressed container.
//
// Before the fix, the adapter bridging the public byte-oriented Transport to
// the internal xfer decoder hardcoded the compressed framing. Against a real
// fossil server it read the first four bytes of the plain card text
// (`prag`... from `pragma server-version ...`) as a big-endian length prefix,
// so decode failed immediately with "declared container length ... exceeds ...
// bytes" — round one, before a single card was parsed.
//
// This drives a genuine clone against a real fossil-served repository and is
// skipped cleanly when no fossil binary is present.
func TestCloneFromRealFossilUncompressedReply(t *testing.T) {
	bin := testutil.RequireFossilBin(t)

	dir := t.TempDir()
	srcRepo := filepath.Join(dir, "source.fossil")

	// Build a small real fossil repository with content to clone.
	runFossil(t, bin, dir, "init", srcRepo)

	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	runFossil(t, bin, work, "open", srcRepo)
	if err := os.WriteFile(filepath.Join(work, "hello.txt"), []byte("hello from real fossil\n"), 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	runFossil(t, bin, work, "add", "hello.txt")
	runFossil(t, bin, work, "commit", "-m", "initial commit", "--no-warnings")

	// Serve it over HTTP. --localhost auto-authenticates localhost requests as
	// the setup user, so an anonymous clone has read access.
	port := freeTCPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := exec.CommandContext(ctx, bin, "server", srcRepo,
		"--localhost", "--port", fmt.Sprintf("%d", port))
	server.Stdout = os.Stderr
	server.Stderr = os.Stderr
	if err := server.Start(); err != nil {
		t.Fatalf("start fossil server: %v", err)
	}
	// Kill signals but does not reap; wait for the exit so the server is not
	// still writing into the temp dir when cleanup removes it.
	defer func() {
		_ = server.Process.Kill()
		_ = server.Wait()
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitTCP(t, addr)

	// Clone through the public HTTP transport — the path that carried the bug.
	url := "http://" + addr + "/"
	clonePath := filepath.Join(dir, "clone.fossil")
	repo, result, err := libfossil.Clone(ctx, clonePath, libfossil.NewHTTPTransport(url), libfossil.CloneOpts{})
	if err != nil {
		t.Fatalf("clone from real fossil server failed: %v", err)
	}
	defer repo.Close()

	if result.Rounds < 1 {
		t.Fatalf("clone reported %d rounds, want >= 1", result.Rounds)
	}
	if result.BlobsRecvd < 1 {
		t.Fatalf("clone received %d blobs, want >= 1 (the committed artifacts)", result.BlobsRecvd)
	}
}

// runFossil runs a fossil subcommand in dir and fails the test on error.
func runFossil(t *testing.T, bin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fossil %v: %v\n%s", args, err, out)
	}
}

// freeTCPPort reserves an ephemeral TCP port and returns it.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitTCP dials addr until it accepts or the deadline passes.
func waitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %s never accepted", addr)
}
