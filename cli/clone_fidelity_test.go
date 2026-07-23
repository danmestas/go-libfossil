package cli_test

import (
	"bytes"
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

	libfossil "github.com/danmestas/go-libfossil"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// serveRepoCtx starts this library's serve stack over repoPath and returns its
// address once it accepts connections, shutting the server down via context
// cancellation at test end. Unlike serveRepo it does not signal the process,
// so it is safe to stand up more than one server within a single test (a
// clone -> serve -> reclone chain needs exactly that). See the SIGINT caveat
// at the top of repo_serve_test.go.
func serveRepoCtx(t *testing.T, repoPath string) string {
	t.Helper()
	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("Open %s: %v", repoPath, err)
	}

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.ServeHTTP(ctx, addr) }()
	t.Cleanup(func() {
		cancel()
		<-done
		r.Close()
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

// repoSQLInt runs a scalar integer query against repoPath through the canonical
// fossil binary's sqlite3 shell.
func repoSQLInt(t *testing.T, bin, repoPath, query string) int64 {
	t.Helper()
	out, err := exec.Command(bin, "sqlite3", "--no-repository", repoPath, query).Output()
	if err != nil {
		t.Fatalf("sqlite3 %q on %s: %v", query, repoPath, err)
	}
	n, err := strconv.ParseInt(string(bytes.TrimSpace(out)), 10, 64)
	if err != nil {
		t.Fatalf("unparseable result %q for %q: %v", out, query, err)
	}
	return n
}

// fossilTipUUID returns the UUID of the newest check-in in repoPath, as the
// canonical fossil binary sees it. Comparing this across clone generations is
// the "pinned tip" check #114 asks for: a second-generation clone that
// diverges reports a different tip here.
func fossilTipUUID(t *testing.T, bin, repoPath string) string {
	t.Helper()
	out, err := exec.Command(bin, "sqlite3", "--no-repository", repoPath,
		"SELECT uuid FROM blob b JOIN event e ON e.objid=b.rid "+
			"WHERE e.type='ci' ORDER BY e.mtime DESC, b.rid DESC LIMIT 1;").Output()
	if err != nil {
		t.Fatalf("tip uuid in %s: %v", repoPath, err)
	}
	return string(bytes.TrimSpace(out))
}

// blobStoreBytes reports the total size of the blob content column, which is
// the store size #114 measures ("store balloons 5x"). File size alone includes
// SQLite free pages, so SUM(length(content)) is the faithful proxy for how
// much blob payload a repository actually holds on disk.
func blobStoreBytes(t *testing.T, bin, repoPath string) int64 {
	t.Helper()
	return repoSQLInt(t, bin, repoPath,
		"SELECT COALESCE(SUM(length(content)),0) FROM blob;")
}

// deltaRowCount reports how many blobs are stored as deltas against another
// blob. This is the number that decides whether a corpus exercises the serve
// side's deltified-row path at all: the handler only takes its expand branch
// for rows content.DeltaSource names, so a corpus with zero delta rows makes
// every fidelity assertion below vacuous for that path.
func deltaRowCount(t *testing.T, bin, repoPath string) int64 {
	t.Helper()
	return repoSQLInt(t, bin, repoPath, "SELECT COUNT(*) FROM delta;")
}

// evolvingFileContent is the content of one file at one generation: a large,
// highly compressible body that grows by one line per generation, so each
// version differs from its predecessor by a little. This is what makes the
// canonical fossil binary store the older version as a delta against the newer
// one -- the delta-chain content #114 worried a re-clone would re-expand and
// never re-encode.
func evolvingFileContent(file, generations int) string {
	var sb strings.Builder
	for line := range 900 {
		fmt.Fprintf(&sb, "file %02d line %05d: the quick brown fox jumps over the lazy dog\n",
			file, line)
	}
	for gen := range generations {
		fmt.Fprintf(&sb, "appended generation %d for file %02d\n", gen, file)
	}
	return sb.String()
}

// buildDeltaBearingRepo builds a repository with the canonical fossil binary
// and returns its path alongside the final content of every file it holds.
//
// The fossil binary is used deliberately rather than this library's own Commit:
// Commit does not deltify, so a corpus built through it holds zero delta rows
// and cannot exercise the serve side's deltified-row path at all. Building the
// source the way a real fossil repository is built is what makes this test a
// real test of #114 rather than a test of the verbatim path twice.
func buildDeltaBearingRepo(t *testing.T, bin, dir string, fileCount, generations int) (string, map[string]string) {
	t.Helper()
	repoPath := filepath.Join(dir, "source.fossil")
	work := filepath.Join(dir, "source-work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", work, err)
	}
	run := func(wd string, args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = wd
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fossil %v in %s: %v\n%s", args, wd, err, out)
		}
	}

	run(dir, "init", repoPath)
	run(work, "open", repoPath)
	for gen := 1; gen <= generations; gen++ {
		for file := range fileCount {
			name := fmt.Sprintf("file%03d.txt", file)
			body := evolvingFileContent(file, gen)
			if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			if gen == 1 {
				run(work, "add", name)
			}
		}
		run(work, "commit", "-m", fmt.Sprintf("generation %d", gen), "--no-warnings")
	}
	run(work, "close")

	want := make(map[string]string, fileCount)
	for file := range fileCount {
		want[fmt.Sprintf("file%03d.txt", file)] = evolvingFileContent(file, generations)
	}
	return repoPath, want
}

// goClone clones srcPath (served by this library's own serve stack) into
// dstPath using this library's own clone, returning how long the clone took.
func goClone(t *testing.T, srcPath, dstPath string) time.Duration {
	t.Helper()
	addr := serveRepoCtx(t, srcPath)
	transport := libfossil.NewHTTPTransport("http://" + addr)
	start := time.Now()
	r, _, err := libfossil.Clone(context.Background(), dstPath, transport, libfossil.CloneOpts{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("go clone %s -> %s: %v", srcPath, dstPath, err)
	}
	r.Close()
	return elapsed
}

// assertCheckoutMatches opens repoPath with the canonical fossil binary and
// compares every checked-out file against want. #114's primary reported defect
// was wrong *checked-out tree content*, which no store-size or tip comparison
// can detect: a repository can hold the right artifact count and still hand a
// checkout the wrong bytes.
func assertCheckoutMatches(t *testing.T, bin, repoPath string, want map[string]string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", work, err)
	}
	cmd := exec.Command(bin, "open", repoPath)
	cmd.Dir = work
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fossil open %s: %v\n%s", repoPath, err, out)
	}

	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("read checkout dir: %v", err)
	}
	checkedOut := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".fslckout") || strings.HasPrefix(e.Name(), "_FOSSIL_") {
			continue
		}
		checkedOut[e.Name()] = true
	}

	for name, wantBody := range want {
		if !checkedOut[name] {
			t.Errorf("checkout of %s is missing %s", repoPath, name)
			continue
		}
		got, err := os.ReadFile(filepath.Join(work, name))
		if err != nil {
			t.Errorf("read %s from checkout: %v", name, err)
			continue
		}
		if string(got) != wantBody {
			t.Errorf("checked-out %s differs from source: got %d bytes, want %d bytes",
				name, len(got), len(wantBody))
		}
	}
	for name := range checkedOut {
		if _, ok := want[name]; !ok {
			t.Errorf("checkout of %s holds unexpected file %s", repoPath, name)
		}
	}
}

// TestCloneServeReclone is the regression guard for #114: clone a corpus, serve
// that clone with this library, reclone from it, and compare the second-
// generation clone's tip, artifact count, blob-store size and checked-out tree
// content against the source. #114 reported the store ballooning ~5x with a
// diverged tip and wrong checked-out content.
//
// The source is built with the canonical fossil binary specifically so it holds
// delta rows, which the test asserts before measuring anything. That assertion
// is the point: the serve side expands a deltified row and sends it as a plain
// cfile (see the comment at internal/sync/handler.go's clone batch loop), so a
// corpus with no delta rows never reaches that branch and every number below
// would come from the verbatim path alone -- a green result proving nothing.
func TestCloneServeReclone(t *testing.T) {
	bin := requireFossilBin(t)
	tmp := t.TempDir()

	srcPath, wantTree := buildDeltaBearingRepo(t, bin, tmp, 6, 10)

	srcDeltas := deltaRowCount(t, bin, srcPath)
	if srcDeltas == 0 {
		t.Fatalf("source corpus holds no delta rows: the serve side's "+
			"deltified-row path is never exercised, so this test would pass "+
			"without testing what #114 is about (blobs=%d)",
			artifactCount(t, bin, srcPath))
	}

	srcArtifacts := artifactCount(t, bin, srcPath)
	srcTip := fossilTipUUID(t, bin, srcPath)
	srcStore := blobStoreBytes(t, bin, srcPath)
	t.Logf("SOURCE: artifacts=%d deltas=%d tip=%s store=%d bytes",
		srcArtifacts, srcDeltas, srcTip, srcStore)

	gen1Path := filepath.Join(tmp, "gen1.fossil")
	gen1Dur := goClone(t, srcPath, gen1Path)
	gen1Artifacts := artifactCount(t, bin, gen1Path)
	gen1Deltas := deltaRowCount(t, bin, gen1Path)
	gen1Tip := fossilTipUUID(t, bin, gen1Path)
	gen1Store := blobStoreBytes(t, bin, gen1Path)
	t.Logf("GEN1:   artifacts=%d deltas=%d tip=%s store=%d bytes  cloned in %s",
		gen1Artifacts, gen1Deltas, gen1Tip, gen1Store, gen1Dur)

	gen2Path := filepath.Join(tmp, "gen2.fossil")
	gen2Dur := goClone(t, gen1Path, gen2Path)
	gen2Artifacts := artifactCount(t, bin, gen2Path)
	gen2Deltas := deltaRowCount(t, bin, gen2Path)
	gen2Tip := fossilTipUUID(t, bin, gen2Path)
	gen2Store := blobStoreBytes(t, bin, gen2Path)
	t.Logf("GEN2:   artifacts=%d deltas=%d tip=%s store=%d bytes  cloned in %s",
		gen2Artifacts, gen2Deltas, gen2Tip, gen2Store, gen2Dur)

	if srcStore > 0 {
		t.Logf("STORE RATIO gen1/source=%.3f gen2/source=%.3f",
			float64(gen1Store)/float64(srcStore), float64(gen2Store)/float64(srcStore))
	}

	if integ, err := exec.Command(bin, "test-integrity", "-R", gen2Path).CombinedOutput(); err != nil {
		t.Errorf("gen2 fossil test-integrity failed: %v\n%s", err, integ)
	}

	if gen1Tip != srcTip {
		t.Errorf("gen1 tip %s diverged from source tip %s", gen1Tip, srcTip)
	}
	if gen2Tip != srcTip {
		t.Errorf("gen2 tip %s diverged from source tip %s", gen2Tip, srcTip)
	}
	if gen1Artifacts != srcArtifacts {
		t.Errorf("gen1 holds %d artifacts, source held %d", gen1Artifacts, srcArtifacts)
	}
	if gen2Artifacts != srcArtifacts {
		t.Errorf("gen2 holds %d artifacts, source held %d", gen2Artifacts, srcArtifacts)
	}

	// A clone must re-deltify what it receives. The serve side hands over every
	// deltified row expanded, so a receiver that did not re-encode would hold a
	// fully expanded store -- #114's balloon. Losing this is the specific
	// regression that would make the ratio assertions below start failing.
	if gen1Deltas == 0 {
		t.Errorf("gen1 holds no delta rows though the source held %d; the "+
			"clone receive path stopped re-deltifying", srcDeltas)
	}
	if gen2Deltas == 0 {
		t.Errorf("gen2 holds no delta rows though the source held %d; the "+
			"clone receive path stopped re-deltifying", srcDeltas)
	}

	// A second-generation clone must not balloon the store. Allow a small
	// margin for legitimate re-compression differences; #114 saw ~5x.
	if srcStore > 0 && float64(gen1Store) > 1.5*float64(srcStore) {
		t.Errorf("gen1 store %d ballooned past 1.5x source store %d", gen1Store, srcStore)
	}
	if srcStore > 0 && float64(gen2Store) > 1.5*float64(srcStore) {
		t.Errorf("gen2 store %d ballooned past 1.5x source store %d", gen2Store, srcStore)
	}

	// The defect #114 named first: what a checkout of the re-cloned repository
	// actually contains.
	assertCheckoutMatches(t, bin, gen2Path, wantTree)
}
