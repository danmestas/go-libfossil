package libfossil

import (
	"errors"
	"path/filepath"
	"testing"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// checkoutAt opens a checkout of r on rid; the test closes it.
func checkoutAt(t *testing.T, r *Repo, rid int64) *Checkout {
	t.Helper()
	co, err := r.CreateCheckout(filepath.Join(t.TempDir(), "co"), CheckoutCreateOpts{})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	t.Cleanup(func() { _ = co.Close() })
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatalf("Extract %d: %v", rid, err)
	}
	return co
}

func TestCheckoutResolveVersion_Relative(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "a", []FileToCommit{{Name: "f", Content: []byte("a\n")}})
	b := commit(t, r, a, "b", []FileToCommit{{Name: "f", Content: []byte("b\n")}})
	c := commit(t, r, b, "c", []FileToCommit{{Name: "f", Content: []byte("c\n")}})
	co := checkoutAt(t, r, b)

	for name, want := range map[string]int64{
		"current": b, "prev": a, "previous": a, "next": c, "tip": c,
	} {
		got, err := co.ResolveVersion(name)
		if err != nil {
			t.Errorf("Checkout.ResolveVersion(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("Checkout.ResolveVersion(%q) = %d, want %d", name, got, want)
		}
	}
	// Without a checkout the keywords are ordinary names, which match nothing.
	if _, err := r.ResolveVersion("current"); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("Repo.ResolveVersion(current) error = %v, want ErrVersionNotFound", err)
	}
}

// A keyword that names nothing falls through to the other forms: at a root,
// "prev" is looked up as a tag.
func TestCheckoutResolveVersion_KeywordFallsThrough(t *testing.T) {
	r := newTestRepo(t)
	a := commit(t, r, 0, "a", []FileToCommit{{Name: "f", Content: []byte("a\n")}})
	tagged := commitBranch(t, r, a, "prev", "tagged prev",
		[]FileToCommit{{Name: "f", Content: []byte("p\n")}})
	co := checkoutAt(t, r, a)

	got, err := co.ResolveVersion("prev")
	if err != nil {
		t.Fatalf("Checkout.ResolveVersion(prev): %v", err)
	}
	if got != tagged {
		t.Errorf("prev at a root = %d, want %d, the check-in tagged prev", got, tagged)
	}
}

func TestResolveBranchStart(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "base", []FileToCommit{{Name: "f", Content: []byte("0\n")}})
	b1 := makeBranch(t, r, base, "feat")
	b2 := commit(t, r, b1, "b2", []FileToCommit{{Name: "f", Content: []byte("2\n")}})
	b2uuid, err := r.UUIDFromRID(b2)
	if err != nil {
		t.Fatal(err)
	}

	// The plain name and the hash both walk back to the branch's start.
	for _, name := range []string{"feat", b2uuid, "tip"} {
		got, err := r.ResolveBranchStart(name)
		if err != nil {
			t.Errorf("ResolveBranchStart(%q): %v", name, err)
			continue
		}
		if got != b1 {
			t.Errorf("ResolveBranchStart(%q) = %d, want %d, feat's first check-in", name, got, b1)
		}
	}

	files, err := r.ListFiles(b2)
	if err != nil || len(files) != 1 {
		t.Fatalf("ListFiles: %v, %d files", err, len(files))
	}
	if _, err := r.ResolveBranchStart(files[0].UUID); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("ResolveBranchStart(file hash) error = %v, want ErrVersionNotFound", err)
	}
}
