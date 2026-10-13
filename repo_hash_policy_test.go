package libfossil

import (
	"path/filepath"
	"testing"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// SetHashPolicy takes the policy names of artifact-format-01 Table 7 and
// stores their ordinal; a wrong-case name is refused and changes nothing.
func TestSetHashPolicy(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "test.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.SetHashPolicy("sha3-only"); err != nil {
		t.Fatalf("SetHashPolicy(sha3-only): %v", err)
	}
	if err := r.SetHashPolicy("SHA1"); err == nil {
		t.Fatal("SetHashPolicy(SHA1) succeeded, want an error")
	}
	got, err := r.Config("hash-policy")
	if err != nil {
		t.Fatal(err)
	}
	if got != "3" {
		t.Errorf("hash-policy = %q, want 3", got)
	}
}
