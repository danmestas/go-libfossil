package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// CLI discovery must use the library's marker rules, including private sparse
// state and ambiguous-marker refusal, rather than silently treating it as dense.
func TestCheckoutDBPathPrivateMarker(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, ".libfossil-checkout")
	if err := os.WriteFile(private, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := checkoutDBPath(dir)
	if err != nil || got != private {
		t.Fatalf("private marker discovery: %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fslckout"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := checkoutDBPath(dir); err == nil {
		t.Fatal("ambiguous native/private markers were silently accepted")
	}
}
