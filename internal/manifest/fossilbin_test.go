package manifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
)

// TestFossilBinaryReadsDeltifiedRepo is the acceptance check that our own
// tests cannot provide: a repository we wrote, with delta chains produced by
// the commit path, must be readable by canonical Fossil. `fossil rebuild`
// re-derives every table from the blob content, so it expands every artifact
// and re-verifies each against its UUID -- if any delta we wrote were
// malformed or linked wrongly, it fails there.
func TestFossilBinaryReadsDeltifiedRepo(t *testing.T) {
	// A skip is invisible without -v, so a run with no fossil binary would
	// otherwise be byte-identical to a passing one -- for the single
	// criterion our own tests cannot substitute for. CI sets
	// REQUIRE_FOSSIL_BIN=1 to turn a missing binary into a failure.
	bin, err := exec.LookPath("fossil")
	if err != nil {
		if os.Getenv("REQUIRE_FOSSIL_BIN") == "1" {
			t.Fatalf("REQUIRE_FOSSIL_BIN=1 but no fossil binary on PATH: %v", err)
		}
		t.Skip("fossil binary not on PATH; cannot verify canonical readability")
	}

	path := filepath.Join(t.TempDir(), "deltified.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	incrementalHistory(t, r, 4, 12, 400)
	s := collectStorageStats(t, r)
	if s.deltaEncoded == 0 {
		t.Fatal("no deltas were produced; this test would prove nothing")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		t.Logf("fossil %s:\n%s", strings.Join(args, " "), out)
		if err != nil {
			t.Fatalf("fossil %s failed: %v", strings.Join(args, " "), err)
		}
		return string(out)
	}

	// test-integrity expands every non-phantom blob and compares the result
	// against its UUID. Run it before rebuild, while the file still holds
	// exactly the bytes we wrote.
	integrity := run("test-integrity", "-R", path)
	if !strings.Contains(integrity, "0 errors") {
		t.Fatalf("fossil test-integrity did not report 0 errors")
	}
	if !strings.Contains(integrity, "low-level database integrity-check: ok") {
		t.Fatalf("fossil reported a low-level database problem")
	}

	// rebuild re-derives every table from blob content, which means walking
	// each delta chain we produced. It verifies the BLOB table unless
	// --noverify is given.
	stats := run("rebuild", path, "--stats")
	if !strings.Contains(stats, "Artifacts:") {
		t.Fatalf("fossil rebuild produced no statistics")
	}
}
