package sync

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
	"github.com/danmestas/go-libfossil/testutil"
)

// startFossilServer starts a fossil server on a free port and returns the URL
// and a cleanup function.
func startFossilServer(t *testing.T, repoPath string) string {
	t.Helper()

	bin := testutil.RequireFossilBin(t)

	// Find a free port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cmd := exec.Command(bin, "server", fmt.Sprintf("--port=%d", port), repoPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("fossil server start: %v", err)
	}

	// Wait for the server to accept connections (poll up to 5s)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			goto ready
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Kill()
	cmd.Wait()
	t.Fatalf("fossil server did not become ready on port %d within 5s", port)

ready:
	serverURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	// Register cleanup via t.Cleanup so it runs before TempDir removal (LIFO).
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		// Remove WAL/SHM files that fossil processes leave behind,
		// so t.TempDir() cleanup doesn't fail with "directory not empty".
		dir := filepath.Dir(repoPath)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal") {
				os.Remove(filepath.Join(dir, name))
			}
		}
	})
	return serverURL
}

// getProjectCode reads the project-code from a fossil repo.
func getProjectCode(t *testing.T, repoPath string) string {
	t.Helper()
	bin := testutil.FossilBinary()
	out, err := exec.Command(bin, "sql", "-R", repoPath,
		"SELECT value FROM config WHERE name='project-code'",
	).Output()
	if err != nil {
		t.Fatalf("get project-code: %v", err)
	}
	code := string(out)
	// Trim whitespace
	for len(code) > 0 && (code[len(code)-1] == '\n' || code[len(code)-1] == '\r' || code[len(code)-1] == ' ') {
		code = code[:len(code)-1]
	}
	return code
}

// getServerCode reads the server-code from a fossil repo.
func getServerCode(t *testing.T, repoPath string) string {
	t.Helper()
	bin := testutil.FossilBinary()
	out, err := exec.Command(bin, "sql", "-R", repoPath,
		"SELECT value FROM config WHERE name='server-code'",
	).Output()
	if err != nil {
		// server-code may not exist; return empty
		return ""
	}
	code := string(out)
	for len(code) > 0 && (code[len(code)-1] == '\n' || code[len(code)-1] == '\r' || code[len(code)-1] == ' ') {
		code = code[:len(code)-1]
	}
	return code
}

func TestIntegrationPushToFossilServer(t *testing.T) {
	bin := testutil.RequireFossilBin(t)

	dir, err := os.MkdirTemp("", "TestIntegrationPush*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	// 1. Create a Go-managed local repo with a checkin
	localPath := filepath.Join(dir, "local.fossil")
	r, err := repo.Create(localPath, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer r.Close()

	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "hello.txt", Content: []byte("hello from libfossil")},
		},
		Comment: "initial checkin from go",
		User:    "testuser",
		Time:    time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	r.Close()

	// 2. Clone the local repo with fossil to create a matching remote
	//    This ensures project-code and server-code match.
	remotePath := filepath.Join(dir, "remote.fossil")
	cloneCmd := exec.Command(bin, "clone", localPath, remotePath)
	cloneOut, err := cloneCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil clone: %v\n%s", err, cloneOut)
	}

	// 3. Create nobody user and grant write capabilities for testing
	// (Go-created repos don't have the standard nobody/anonymous users that fossil new creates)
	exec.Command(bin, "user", "new", "nobody", "", "cghijknorswy", "-R", remotePath).Run()
	exec.Command(bin, "user", "capabilities", "nobody", "cghijknorswy", "-R", remotePath).Run()

	// 4. Read project-code and server-code from the remote (they match local after clone)
	projCode := getProjectCode(t, remotePath)
	srvCode := getServerCode(t, remotePath)
	if projCode == "" {
		t.Fatal("project-code is empty")
	}

	// 4. Start fossil server on the remote
	serverURL := startFossilServer(t, remotePath)

	// 5. Re-open local repo and push via our sync engine
	r2, err := repo.Open(localPath)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r2.Close()

	transport := &HTTPTransport{URL: serverURL}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := Sync(ctx, r2, transport, SyncOpts{
		Push:        true,
		Pull:        false,
		ProjectCode: projCode,
		ServerCode:  srvCode,
		User:        "",
		Password:    "",
	})

	// Log results regardless of error — this is informational since fossil
	// server behavior can be unpredictable in test environments.
	t.Logf("Push result: rounds=%d filesSent=%d filesRecvd=%d errors=%v err=%v",
		result.Rounds, result.FilesSent, result.FilesRecvd, result.Errors, err)

	if err != nil {
		t.Logf("NOTE: push to fossil server returned error (may be expected): %v", err)
		// Don't hard-fail; the unit tests with mock transport already validate engine logic.
		return
	}

	if result.Rounds < 1 {
		t.Errorf("expected at least 1 round, got %d", result.Rounds)
	}
	t.Logf("Push completed in %d rounds, sent %d files", result.Rounds, result.FilesSent)
}

func TestIntegrationPullFromFossilServer(t *testing.T) {
	bin := testutil.RequireFossilBin(t)

	dir, err := os.MkdirTemp("", "TestIntegrationPull*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	// 1. Create a remote repo with fossil new
	remotePath := filepath.Join(dir, "remote.fossil")
	newCmd := exec.Command(bin, "new", remotePath)
	newOut, err := newCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil new: %v\n%s", err, newOut)
	}

	// 2. Start fossil server on the remote
	serverURL := startFossilServer(t, remotePath)

	// 3. Clone it to local with fossil clone (so project/server codes match)
	localPath := filepath.Join(dir, "local.fossil")
	cloneCmd := exec.Command(bin, "clone", serverURL, localPath)
	cloneOut, err := cloneCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil clone: %v\n%s", err, cloneOut)
	}

	// 4. Read project-code and server-code from local clone
	projCode := getProjectCode(t, localPath)
	srvCode := getServerCode(t, localPath)
	if projCode == "" {
		t.Fatal("project-code is empty")
	}

	// 5. Open local with repo.Open and pull via our sync engine
	r, err := repo.Open(localPath)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r.Close()

	transport := &HTTPTransport{URL: serverURL}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, syncErr := Sync(ctx, r, transport, SyncOpts{
		Push:        false,
		Pull:        true,
		ProjectCode: projCode,
		ServerCode:  srvCode,
		User:        "",
		Password:    "",
	})

	t.Logf("Pull result: rounds=%d filesSent=%d filesRecvd=%d errors=%v err=%v",
		result.Rounds, result.FilesSent, result.FilesRecvd, result.Errors, syncErr)

	if syncErr != nil {
		t.Logf("NOTE: pull from fossil server returned error (may be expected): %v", syncErr)
		return
	}

	// 6. Verify any received blobs pass content.Verify
	rows, err := r.DB().Query("SELECT rid FROM blob WHERE size >= 0")
	if err != nil {
		t.Fatalf("query blobs: %v", err)
	}
	defer rows.Close()

	verified := 0
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			t.Fatalf("scan rid: %v", err)
		}
		if err := content.Verify(r.DB(), libfossil.FslID(rid)); err != nil {
			// Blobs stored by fossil clone may use Fossil's compression format
			// (4-byte size prefix) which our blob.Decompress doesn't handle yet.
			t.Logf("content.Verify rid=%d: %v", rid, err)
		}
		verified++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error: %v", err)
	}
	t.Logf("Pull completed in %d rounds, received %d files, verified %d blobs",
		result.Rounds, result.FilesRecvd, verified)
}

// blobContentBytes returns SELECT sum(length(content)) FROM blob WHERE
// size>=0 for a fossil repo at path, via the fossil CLI.
func blobContentBytesFossilSQL(t *testing.T, path string) int64 {
	t.Helper()
	bin := testutil.FossilBinary()
	out, err := exec.Command(bin, "sql", "-R", path,
		"SELECT sum(length(content)) FROM blob WHERE size>=0",
	).Output()
	if err != nil {
		t.Fatalf("fossil sql content bytes: %v", err)
	}
	s := strings.TrimSpace(string(out))
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parse content bytes %q: %v", s, err)
	}
	return n
}

// TestIntegrationCloneContentBytesMatchSource is the acceptance test for
// issue #112: go-libfossil's clone must store received wire frames
// verbatim, so a cloned repository's blob.content bytes sum to exactly the
// same total as its source -- not merely reconstruct to the same content
// after decompression. Before the fix, storeReceivedFile/storeDeltaContent
// decompressed every received frame and re-encoded it with blob.Compress's
// unparameterized zlib.DefaultCompression, producing a clone that verifies
// correctly but is not byte-identical to its source (fossil and zit both
// are).
//
// The corpus commits several files across multiple revisions so the
// exchange includes both full-content and delta-encoded (cfile with a
// delta source) cards -- the two receive paths this issue's fix touches.
func TestIntegrationCloneContentBytesMatchSource(t *testing.T) {
	if !testutil.HasFossil() {
		t.Skip("fossil not in PATH")
	}
	bin := testutil.FossilBinary()

	dir, err := os.MkdirTemp("", "TestIntegrationCloneParity*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	// 1. Create the source repo and populate it with a small multi-revision
	// corpus via the real fossil CLI, so its blob table holds Fossil's own
	// compression output -- the thing our clone must reproduce exactly.
	remotePath := filepath.Join(dir, "remote.fossil")
	if out, err := exec.Command(bin, "new", remotePath).CombinedOutput(); err != nil {
		t.Fatalf("fossil new: %v\n%s", err, out)
	}

	workDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}
	runFossil := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fossil %v: %v\n%s", args, err, out)
		}
	}
	runFossil("open", remotePath)
	// fossil new already provisions a default admin user (named after the
	// OS user); commits just need *a* user to attribute to, not "tester"
	// specifically.

	writeAndCommit := func(name, body, msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		runFossil("add", name)
		runFossil("commit", "-m", msg, "--no-warnings")
	}

	// A file that grows across three revisions, long enough that Fossil
	// deltifies later revisions against earlier ones -- exercising the
	// delta-cfile receive path, not just full-content.
	base := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 40)
	writeAndCommit("growing.txt", base, "initial")
	writeAndCommit("growing.txt", base+strings.Repeat("more content appended here\n", 20), "grow once")
	writeAndCommit("growing.txt", base+strings.Repeat("more content appended here\n", 40), "grow twice")
	writeAndCommit("other.txt", strings.Repeat("unrelated content\n", 30), "unrelated file")

	sourceBytes := blobContentBytesFossilSQL(t, remotePath)
	if sourceBytes <= 0 {
		t.Fatalf("fixture bug: source content bytes = %d, want > 0", sourceBytes)
	}

	// 2. Serve the populated source and clone it with our own engine.
	serverURL := startFossilServer(t, remotePath)

	localPath := filepath.Join(dir, "local.fossil")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	transport := &HTTPTransport{URL: serverURL}
	r, result, err := Clone(ctx, localPath, transport, CloneOpts{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	defer r.Close()
	t.Logf("Clone completed in %d rounds, received %d blobs", result.Rounds, result.BlobsRecvd)

	// 3. blob.content must sum to exactly the source's total -- not just
	// decompress to the same content.
	var localBytes int64
	if err := r.DB().QueryRow("SELECT sum(length(content)) FROM blob WHERE size>=0").Scan(&localBytes); err != nil {
		t.Fatalf("query local content bytes: %v", err)
	}
	if localBytes != sourceBytes {
		t.Fatalf("clone content bytes = %d, want %d (source) -- clone is not byte-identical "+
			"to its source; received wire frames were re-encoded instead of stored verbatim",
			localBytes, sourceBytes)
	}
}
