package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type TestRepo struct {
	Path string
	Dir  string
}

// FossilBinary resolves the canonical fossil binary: the FOSSIL_BIN override
// first, then PATH. An override that does not resolve to an executable is
// treated as no binary at all, so the caller reports "fossil not found" rather
// than letting a stale path surface later as a raw fork/exec error.
func FossilBinary() string {
	if bin := os.Getenv("FOSSIL_BIN"); bin != "" {
		path, err := exec.LookPath(bin)
		if err != nil {
			return ""
		}
		return path
	}
	path, err := exec.LookPath("fossil")
	if err != nil {
		return ""
	}
	return path
}

// RequireFossilBin resolves the fossil binary for tests that need canonical
// Fossil to run. It resolves the same way FossilBinary does (the FOSSIL_BIN
// override, then PATH). When no binary is found it skips the test, unless
// REQUIRE_FOSSIL_BIN=1 is set -- then it fails. That makes CI turn a missing
// binary into a loud failure (verifying real Fossil reads what we write is the
// one thing our own tests cannot substitute for) while local runs without
// fossil installed stay an opt-in skip.
func RequireFossilBin(t *testing.T) string {
	t.Helper()
	if bin := FossilBinary(); bin != "" {
		return bin
	}
	where := "set FOSSIL_BIN or install fossil on PATH"
	if bin := os.Getenv("FOSSIL_BIN"); bin != "" {
		where = fmt.Sprintf("FOSSIL_BIN=%q is not an executable", bin)
	}
	if os.Getenv("REQUIRE_FOSSIL_BIN") == "1" {
		t.Fatalf("REQUIRE_FOSSIL_BIN=1 but no fossil binary found (%s)", where)
	}
	t.Skipf("fossil binary not found (%s); set REQUIRE_FOSSIL_BIN=1 to require it", where)
	return ""
}

func NewTestRepo(t *testing.T) *TestRepo {
	t.Helper()
	bin := RequireFossilBin(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")
	cmd := exec.Command(bin, "new", path)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil new failed: %v\n%s", err, out)
	}
	return &TestRepo{Path: path, Dir: dir}
}

func NewTestRepoFromPath(t *testing.T, path string) *TestRepo {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("cannot resolve path %q: %v", path, err)
	}
	return &TestRepo{Path: abs, Dir: filepath.Dir(abs)}
}

func (r *TestRepo) FossilRebuild(t *testing.T) {
	t.Helper()
	cmd := exec.Command(FossilBinary(), "rebuild", r.Path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}
}

func (r *TestRepo) FossilArtifact(t *testing.T, uuid string) []byte {
	t.Helper()
	cmd := exec.Command(FossilBinary(), "artifact", uuid, "-R", r.Path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fossil artifact %s failed: %v", uuid, err)
	}
	return out
}

func FossilRebuild(repoPath string) error {
	cmd := exec.Command(FossilBinary(), "rebuild", repoPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fossil rebuild: %v\n%s", err, out)
	}
	return nil
}

func FossilTimeline(repoPath string) ([]byte, error) {
	cmd := exec.Command(FossilBinary(), "timeline", "-n", "100", "-R", repoPath)
	return cmd.Output()
}

func FossilArtifactByPath(repoPath, uuid string) ([]byte, error) {
	cmd := exec.Command(FossilBinary(), "artifact", uuid, "-R", repoPath)
	return cmd.Output()
}

func (r *TestRepo) FossilSQL(t *testing.T, sql string) string {
	t.Helper()
	cmd := exec.Command(FossilBinary(), "sql", "-R", r.Path, sql)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("fossil sql failed: %v\nstderr: %s", err, exitErr.Stderr)
		}
		t.Fatalf("fossil sql failed: %v", err)
	}
	return strings.TrimSpace(string(out))
}
