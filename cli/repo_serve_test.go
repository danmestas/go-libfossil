package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	libfossil "github.com/danmestas/go-libfossil"
	"github.com/danmestas/go-libfossil/cli"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/testutil"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// NOTE: TestRepoServeCmdRun and every test that goes through serveRepo send
// a real SIGINT to the test binary's own process (syscall.Kill) to exercise
// RepoServeCmd's Ctrl-C shutdown path end-to-end, rather than faking it by
// cancelling an injected context. That is only safe because
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
	bin := requireFossilBin(t)

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

	addr := serveRepo(t, repoPath)

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

// requireFossilBin returns the canonical fossil binary, skipping the test if
// it is absent. A skip is invisible without -v, so a run with no fossil
// binary would otherwise be byte-identical to a passing one -- for the single
// criterion our own transport cannot substitute for. CI sets
// REQUIRE_FOSSIL_BIN=1 to turn a missing binary into a failure (issue #86).
func requireFossilBin(t *testing.T) string {
	t.Helper()
	bin := testutil.FossilBinary()
	if bin == "" {
		if os.Getenv("REQUIRE_FOSSIL_BIN") == "1" {
			t.Fatal("REQUIRE_FOSSIL_BIN=1 but no fossil binary on PATH")
		}
		t.Skip("fossil binary not on PATH; cannot verify canonical interoperability")
	}
	if out, err := exec.Command(bin, "version").CombinedOutput(); err == nil {
		t.Logf("using canonical fossil binary %s: %s", bin, bytes.TrimSpace(out))
	} else {
		t.Logf("using canonical fossil binary %s (version check failed: %v)", bin, err)
	}
	return bin
}

// serveRepo starts the CLI serve command over repoPath and returns its
// address once it accepts connections. The server is shut down at test end.
func serveRepo(t *testing.T, repoPath string) string {
	t.Helper()
	addr := freeAddr(t)
	g := &cli.Globals{Repo: repoPath}
	c := &cli.RepoServeCmd{Addr: addr}

	done := make(chan error, 1)
	go func() {
		done <- c.Run(g)
	}()
	t.Cleanup(func() {
		// SIGINT is Run's shutdown path only while its signal.NotifyContext
		// is installed. If Run has already returned, the handler is gone and
		// SIGINT's default action kills the whole test binary -- turning a
		// server failure into a truncated process death with no verdict.
		// Report that case instead of signalling into it.
		select {
		case err := <-done:
			t.Errorf("serve exited before the test finished: %v", err)
			return
		default:
		}
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-done
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", dialErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeCorpus commits payloadBytes of pseudo-random file content into r,
// fileBytes per file and filesPerCommit files per commit. Content is
// incompressible and per-file distinct so nothing collapses into a shared
// blob, which is what makes the corpus size a faithful proxy for the wire
// bytes a clone of it must carry.
func writeCorpus(t *testing.T, r *libfossil.Repo, payloadBytes, fileBytes, filesPerCommit int) {
	t.Helper()
	if payloadBytes <= 0 || fileBytes <= 0 || filesPerCommit <= 0 {
		t.Fatalf("writeCorpus: sizes must be positive, got %d/%d/%d",
			payloadBytes, fileBytes, filesPerCommit)
	}
	files := (payloadBytes + fileBytes - 1) / fileBytes
	rng := rand.New(rand.NewSource(1))

	batch := make([]libfossil.FileToCommit, 0, filesPerCommit)
	flush := func(n int) {
		if len(batch) == 0 {
			return
		}
		if _, _, err := r.Commit(libfossil.CommitOpts{
			Files:   batch,
			Comment: fmt.Sprintf("corpus commit %d", n),
			User:    "test",
		}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		batch = batch[:0]
	}
	for i := range files {
		content := make([]byte, fileBytes)
		if _, err := rng.Read(content); err != nil {
			t.Fatalf("rng.Read: %v", err)
		}
		batch = append(batch, libfossil.FileToCommit{
			Name:    fmt.Sprintf("corpus/file%05d.bin", i),
			Content: content,
		})
		if len(batch) == filesPerCommit {
			flush(i / filesPerCommit)
		}
	}
	flush(files / filesPerCommit)
}

// serveCountingClones starts the same server stack `repo serve` runs --
// sync.ServeHTTP over sync.HandleSync -- with a wrapper that counts the clone
// batches the server emits, and returns the address and that counter.
//
// The count is taken on the server because no client-side number is portable.
// fossil prints "Round-trips: N" once per round, carriage-return separated, so
// on 2.23 the first occurrence is always 1 regardless of how many rounds ran;
// 2.28 added a TTY guard that collapses it to a single final line. Parsing that
// output made the assertion mean different things on different clients, and CI
// (2.23) disagreed with this machine (2.28) on an identical server. Counting
// batches the server actually emitted is the property under test anyway.
//
// This bypasses cli.RepoServeCmd, whose Run is a five-line wrapper around the
// same ServeHTTP call. TestRepoServeCmdCanonicalFossilClone still drives the
// command itself end to end.
func serveCountingClones(t *testing.T, repoPath string) (addr string, batches *atomic.Int64) {
	t.Helper()
	src, err := repo.Open(repoPath)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	t.Cleanup(func() { src.Close() })

	batches = &atomic.Int64{}
	h := func(ctx context.Context, r *repo.Repo, req *xfer.Message) (*xfer.Message, error) {
		for _, c := range req.Cards {
			if _, ok := c.(*xfer.CloneCard); ok {
				batches.Add(1)
				break
			}
		}
		return sync.HandleSync(ctx, r, req)
	}

	addr = freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sync.ServeHTTP(ctx, addr, src, h) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return addr, batches
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", dialErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// artifactCount reports the public artifacts a repository holds, as canonical
// fossil counts them.
func artifactCount(t *testing.T, bin, repoPath string) int {
	t.Helper()
	out, err := exec.Command(bin, "sqlite3", "--no-repository", repoPath,
		"SELECT count(*) FROM blob WHERE size>=0;").Output()
	if err != nil {
		t.Fatalf("counting artifacts in %s: %v", repoPath, err)
	}
	n, err := strconv.Atoi(string(bytes.TrimSpace(out)))
	if err != nil {
		t.Fatalf("unparseable artifact count %q: %v", out, err)
	}
	return n
}

// TestRepoServeCmdCanonicalFossilMultiBatchClone is the interop half of #92:
// a real fossil client cloning a corpus from a libfossil server, sized past
// the server's clone pagination boundary so the multi-round path runs.
//
// Method: build a corpus whose content exceeds sync.DefaultCloneBatchBytes
// several times over, serve it, clone with the canonical binary, then assert
// on three independent things -- how many clone batches the server emitted
// (counted server-side; otherwise the pagination path may never have been
// reached and the rest proves nothing), that the clone holds every artifact
// the source held, and that canonical's own `test-integrity` accepts it.
//
// Corpus size is a parameter because the interesting behavior is at the batch
// boundary, not at any particular size.
func TestRepoServeCmdCanonicalFossilMultiBatchClone(t *testing.T) {
	bin := requireFossilBin(t)

	for _, tc := range []struct {
		name         string
		payloadBytes int
		fileBytes    int
		// batchesMax is the discriminating assertion for #88. The corpus of
		// many small artifacts is a fraction of one round's byte budget, so a
		// size-bounded server drains it in a single clone batch no matter how
		// many artifacts it holds. A count-bounded server needs one batch per
		// 200 artifacts, which is the ceiling #88 describes. Zero means no
		// upper bound is asserted.
		batchesMax int
		batchesMin int
	}{
		{"single-batch", sync.DefaultCloneBatchBytes / 4, 64 * 1024, 0, 1},
		{"multi-batch", sync.DefaultCloneBatchBytes * 3, 64 * 1024, 0, 3},
		{"many-small-artifacts", 1 << 20, 512, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			repoPath := filepath.Join(tmp, "serve.fossil")

			r, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: "test"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			writeCorpus(t, r, tc.payloadBytes, tc.fileBytes, 256)
			r.Close()

			sourceArtifacts := artifactCount(t, bin, repoPath)
			addr, batches := serveCountingClones(t, repoPath)

			clonePath := filepath.Join(tmp, "clone.fossil")
			out, err := exec.Command(bin, "clone", "http://"+addr, clonePath).CombinedOutput()
			if err != nil {
				t.Fatalf("canonical fossil clone failed: %v\n%s", err, out)
			}

			got := int(batches.Load())
			t.Logf("corpus %d bytes, %d artifacts, server emitted %d clone batches",
				tc.payloadBytes, sourceArtifacts, got)
			if got < tc.batchesMin {
				t.Errorf("server emitted %d clone batches, want >= %d; the %s path was not exercised",
					got, tc.batchesMin, tc.name)
			}
			if tc.batchesMax > 0 && got > tc.batchesMax {
				t.Errorf("server emitted %d clone batches for %d artifacts, want <= %d; "+
					"it is paginating by artifact count, not output size (#88)",
					got, sourceArtifacts, tc.batchesMax)
			}

			if got := artifactCount(t, bin, clonePath); got != sourceArtifacts {
				t.Errorf("clone holds %d artifacts, source held %d", got, sourceArtifacts)
			}

			integrity, err := exec.Command(bin, "test-integrity", "-R", clonePath).CombinedOutput()
			if err != nil {
				t.Fatalf("fossil test-integrity failed: %v\n%s", err, integrity)
			}
			t.Logf("test-integrity: %s", bytes.TrimSpace(integrity))
		})
	}
}
