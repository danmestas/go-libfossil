package cli_test

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	libfossil "github.com/danmestas/libfossil"
	"github.com/danmestas/libfossil/cli"
	"github.com/danmestas/libfossil/testutil"

	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// NOTE: TestRepoServeCmdRun and TestRepoServeCmdCanonicalFossilClone below
// send a real SIGINT to the test binary's own process (syscall.Kill) to
// exercise RepoServeCmd's Ctrl-C shutdown path end-to-end, rather than
// faking it by cancelling an injected context. That is only safe because
// nothing else in package cli_test is listening for signals or running
// concurrently with them at the same time: no test in this package calls
// t.Parallel(), and no other test installs its own signal.Notify or
// signal.NotifyContext. If a future test in this package adds either, the
// SIGINT sent here can be delivered to (or race with) that unrelated
// handler instead, producing flakiness that is hard to diagnose from the
// failure alone. Keep both invariants true for this package, or convert
// these tests to inject a cancellable context instead of signaling the
// process.

// serveTestCLI mirrors the wiring in cmd/libfossil/main.go closely enough to
// exercise kong flag parsing for RepoServeCmd without depending on the main
// package.
type serveTestCLI struct {
	cli.Globals
	Repo cli.RepoCmd `cmd:""`
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestRepoServeCmdDefaultAddr(t *testing.T) {
	var c serveTestCLI
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"repo", "serve"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Repo.Serve.Addr != "127.0.0.1:8080" {
		t.Errorf("default Addr = %q, want 127.0.0.1:8080", c.Repo.Serve.Addr)
	}
}

func TestRepoServeCmdAddrFlagOverride(t *testing.T) {
	var c serveTestCLI
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"repo", "serve", "--addr", "0.0.0.0:9191"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Repo.Serve.Addr != "0.0.0.0:9191" {
		t.Errorf("Addr = %q, want 0.0.0.0:9191", c.Repo.Serve.Addr)
	}
}

// TestRepoServeCmdRun starts the server via the CLI command, confirms a real
// network round trip against it, then sends SIGINT (as Ctrl-C would) and
// verifies Run returns cleanly rather than hanging.
func TestRepoServeCmdRun(t *testing.T) {
	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "serve.fossil")

	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.Close()

	addr := freeAddr(t)
	g := &cli.Globals{Repo: repoPath}
	c := &cli.RepoServeCmd{Addr: addr}

	done := make(chan error, 1)
	go func() {
		done <- c.Run(g)
	}()

	// Poll until the server accepts connections, then confirm it answers
	// with the xfer server probe page over an actual network round trip.
	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err = http.Get("http://" + addr + "/")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}

	// Ctrl-C: send SIGINT to our own process. signal.NotifyContext inside
	// Run should translate this into context cancellation.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("signal self: %v", err)
	}

	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("Run returned error after interrupt: %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after interrupt (hang)")
	}
}

// TestRepoServeCmdCanonicalFossilClone proves the served repository is
// actually compatible with a stock fossil client, not just this library's
// own transport. Skips if the canonical fossil binary isn't on PATH.
func TestRepoServeCmdCanonicalFossilClone(t *testing.T) {
	if !testutil.HasFossil() {
		t.Skip("fossil not in PATH")
	}
	bin := testutil.FossilBinary()

	// Log which binary and version actually did the cloning. A skip and a
	// pass look identical in a non-verbose CI log; this line is the only
	// evidence in the record that a real canonical fossil process -- not
	// just this library's own transport -- exercised the served endpoint.
	if out, err := exec.Command(bin, "version").CombinedOutput(); err == nil {
		t.Logf("using canonical fossil binary %s: %s", bin, bytes.TrimSpace(out))
	} else {
		t.Logf("using canonical fossil binary %s (version check failed: %v)", bin, err)
	}

	tmp := t.TempDir()
	repoPath := filepath.Join(tmp, "serve.fossil")

	r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, _, err = r.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: "hello.txt", Content: []byte("hello from serve\n")},
		},
		Comment: "initial commit",
		User:    "test",
	})
	if err != nil {
		r.Close()
		t.Fatalf("Commit: %v", err)
	}
	r.Close()

	addr := freeAddr(t)
	g := &cli.Globals{Repo: repoPath}
	c := &cli.RepoServeCmd{Addr: addr}

	done := make(chan error, 1)
	go func() {
		done <- c.Run(g)
	}()
	t.Cleanup(func() {
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-done
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", dialErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	clonePath := filepath.Join(tmp, "clone.fossil")
	cmd := exec.Command(bin, "clone", "http://"+addr, clonePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("canonical fossil clone failed: %v\n%s", err, out)
	}

	if _, err := os.Stat(clonePath); err != nil {
		t.Fatalf("cloned repository missing: %v", err)
	}
}
