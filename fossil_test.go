package libfossil

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func TestCreateAndOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Path() != path {
		t.Errorf("Path() = %q, want %q", r.Path(), path)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatal("repo file should exist after Create")
	}

	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if r2.Path() != path {
		t.Errorf("Path() = %q, want %q", r2.Path(), path)
	}
}

func TestCreateAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	_, err = Create(path, CreateOpts{User: "test"})
	if err == nil {
		t.Error("Create on existing repo should fail")
	}
}

func TestOpenNotFound(t *testing.T) {
	_, err := Open("/nonexistent/path.fossil")
	if err == nil {
		t.Error("Open on nonexistent path should fail")
	}
}

func TestCreate_ProjectCode_Explicit(t *testing.T) {
	const code = "0123456789abcdef0123456789abcdef01234567"
	path := filepath.Join(t.TempDir(), "test.fossil")

	r, err := Create(path, CreateOpts{User: "test", ProjectCode: code})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r2.Close()
	got, err := r2.inner.Config("project-code")
	if err != nil {
		t.Fatalf("Config(project-code): %v", err)
	}
	if got != code {
		t.Errorf("project-code = %q, want %q", got, code)
	}
}

func TestCreate_ProjectCode_Invalid(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"too short", "deadbeef"},
		{"uppercase", strings.Repeat("DEADBEEF", 5)},
		{"non-hex char", strings.Repeat("g", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.fossil")
			_, err := Create(path, CreateOpts{User: "test", ProjectCode: tc.code})
			if err == nil {
				t.Fatalf("Create with ProjectCode=%q should fail", tc.code)
			}
			if !strings.Contains(err.Error(), "invalid CreateOpts.ProjectCode") {
				t.Errorf("error %q missing 'invalid CreateOpts.ProjectCode'", err)
			}
			if _, statErr := os.Stat(path); statErr == nil {
				t.Error("repo file should not exist after rejected Create")
			}
		})
	}
}

// TestCreate_ProjectCode_FossilValidation creates a Go-side repo with an
// explicit ProjectCode, then runs vanilla `fossil rebuild` against it and
// reads the project-code back via `fossil sql` to confirm vanilla Fossil
// accepts the value and stores it as-is.
func TestCreate_ProjectCode_FossilValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping fossil validation")
	}
	const code = "0123456789abcdef0123456789abcdef01234567"
	path := filepath.Join(t.TempDir(), "test.fossil")

	r, err := Create(path, CreateOpts{User: "test", ProjectCode: code})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, err := exec.Command("fossil", "rebuild", path).CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}

	out, err = exec.Command("fossil", "sql", "-R", path,
		"SELECT value FROM config WHERE name='project-code';").Output()
	if err != nil {
		t.Fatalf("fossil sql: %v", err)
	}
	got := string(bytes.Trim(out, "'\n \t"))
	if got != code {
		t.Errorf("project-code via fossil sql = %q, want %q", got, code)
	}
}

func TestCreate_ProjectCode_EmptyGeneratesHex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()

	code, err := r.inner.Config("project-code")
	if err != nil {
		t.Fatalf("Config(project-code): %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(code) {
		t.Errorf("generated project-code %q does not match ^[0-9a-f]{40}$", code)
	}
}
