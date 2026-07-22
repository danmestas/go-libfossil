package testutil

import (
	"os"
	"testing"
)

func TestNewTestRepo(t *testing.T) {
	tr := NewTestRepo(t)
	if _, err := os.Stat(tr.Path); err != nil {
		t.Fatalf("repo file does not exist: %v", err)
	}
}

func TestFossilRebuild(t *testing.T) {
	tr := NewTestRepo(t)
	tr.FossilRebuild(t)
}

func TestFossilSQL(t *testing.T) {
	tr := NewTestRepo(t)
	out := tr.FossilSQL(t, "SELECT count(*) FROM blob;")
	if out != "1" {
		t.Fatalf("FossilSQL count(*) = %q, want %q", out, "1")
	}
}

func TestFossilBinary(t *testing.T) {
	path := RequireFossilBin(t)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fossil binary not found at %q: %v", path, err)
	}
}

// TestRequireFossilBin verifies the found path: when a fossil binary is
// resolvable, RequireFossilBin returns exactly the path FossilBinary resolves
// and neither skips nor fails. The not-found branches (skip vs. Fatalf when
// REQUIRE_FOSSIL_BIN=1) are exercised end-to-end by the CI test step, which
// runs the whole suite with REQUIRE_FOSSIL_BIN=1 against an installed fossil.
func TestRequireFossilBin(t *testing.T) {
	want := FossilBinary()
	if want == "" {
		// Fails under REQUIRE_FOSSIL_BIN=1, skips otherwise.
		RequireFossilBin(t)
	}
	got := RequireFossilBin(t)
	if got != want {
		t.Fatalf("RequireFossilBin = %q, want %q", got, want)
	}
}
