package libfossil

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNewConeNormalizes(t *testing.T) {
	c, err := NewCone([]string{"src/sub", "src", "README"}, []string{"src/vendor"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Include(), []string{"README", "src"}) || !slices.Equal(c.Exclude(), []string{"src/vendor"}) {
		t.Fatalf("cone = %v", c)
	}
	if !c.Covers("src/a.c") || c.Covers("src/vendor/v.c") || c.Covers("docs/x") {
		t.Fatal("Covers disagrees with the members")
	}
	if covered, member := c.Decide("src/vendor/v.c"); covered || member != "src/vendor" {
		t.Fatalf("Decide(src/vendor/v.c) = %v, %q", covered, member)
	}
	if len(c.Digest()) != 64 || c.String() != "README, src, !src/vendor" {
		t.Fatalf("Digest %q, String %q", c.Digest(), c.String())
	}
	if _, err := NewCone([]string{"src"}, []string{"tools"}); err == nil {
		t.Fatal("a dead exclusion was accepted")
	}
	wider, err := c.With([]string{"docs"}, nil)
	if err != nil || !wider.Covers("docs/x") {
		t.Fatalf("With: %v, %v", wider, err)
	}
	narrower, err := wider.Without([]string{"docs", "src/vendor"})
	if err != nil || narrower.Covers("docs/x") || !narrower.Covers("src/vendor/v.c") {
		t.Fatalf("Without: %v, %v", narrower, err)
	}
}

func TestSparseCheckout(t *testing.T) {
	dir := t.TempDir()
	r, err := Create(filepath.Join(dir, "r.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var files []FileToCommit
	for _, name := range []string{"README", "src/a.c", "src/vendor/v.c", "tools/t.c"} {
		files = append(files, FileToCommit{Name: name, Content: []byte(name + "\n")})
	}
	rid, _, err := r.Commit(CommitOpts{Files: files, Comment: "one", User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCone([]string{"src"}, []string{"src/vendor"})
	if err != nil {
		t.Fatal(err)
	}
	coDir := filepath.Join(dir, "work")
	co, err := r.CreateCheckout(coDir, CheckoutCreateOpts{Cone: c})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if err := co.Extract(rid, ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := co.Cone(); got == nil || got.String() != c.String() {
		t.Fatalf("Cone() = %v", got)
	}
	assertPresent(t, coDir, map[string]bool{"src/a.c": true, "README": false, "tools/t.c": false})

	if err := os.WriteFile(filepath.Join(coDir, "x.c"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Add([]string{"x.c"}); !errors.Is(err, ErrOutsideCone) || !strings.Contains(err.Error(), c.String()) {
		t.Fatalf("Add outside the cone: %v", err)
	}

	change, err := co.SetCone(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(change.Materialized, []string{"README", "src/vendor/v.c", "tools/t.c"}) {
		t.Fatalf("densifying materialized %q", change.Materialized)
	}
	if co.Cone() != nil {
		t.Fatal("a dense checkout reports a cone")
	}
	assertPresent(t, coDir, map[string]bool{"README": true, "tools/t.c": true})
}

func assertPresent(t *testing.T, dir string, want map[string]bool) {
	t.Helper()
	for name, present := range want {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != present {
			t.Errorf("%s: present=%v, want %v", name, err == nil, present)
		}
	}
}
