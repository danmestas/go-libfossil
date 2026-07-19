package cli

import (
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestVersionShape verifies Version() always produces the same fixed shape
// -- four whitespace-separated fields -- regardless of what build
// information happens to be available, so a downstream parser can rely on
// strings.Fields(Version()) unconditionally.
func TestVersionShape(t *testing.T) {
	saved := buildVersion
	buildVersion = ""
	defer func() { buildVersion = saved }()

	v := Version()

	if strings.Contains(v, "\n") {
		t.Fatalf("Version() = %q, must be a single line", v)
	}

	fields := strings.Fields(v)
	if len(fields) != 4 {
		t.Fatalf("Version() = %q, want 4 whitespace-separated fields, got %d", v, len(fields))
	}
	if fields[0] != "libfossil" {
		t.Errorf("field[0] = %q, want %q", fields[0], "libfossil")
	}
	if fields[1] == "" {
		t.Errorf("field[1] (version) must not be empty")
	}
	if !strings.HasPrefix(fields[2], "go") {
		t.Errorf("field[2] (Go version) = %q, want prefix %q", fields[2], "go")
	}
	wantPlatform := runtime.GOOS + "/" + runtime.GOARCH
	if fields[3] != wantPlatform {
		t.Errorf("field[3] (platform) = %q, want %q", fields[3], wantPlatform)
	}
}

// TestVersionBuildVersionOverrideWins verifies that a build-time ldflags
// override is used verbatim as the version field, taking priority over
// whatever runtime/debug.ReadBuildInfo would otherwise report -- this is
// the release-build contract described in the ldflags comment on
// buildVersion.
func TestVersionBuildVersionOverrideWins(t *testing.T) {
	saved := buildVersion
	buildVersion = "v9.9.9-test"
	defer func() { buildVersion = saved }()

	fields := strings.Fields(Version())
	if len(fields) != 4 {
		t.Fatalf("Version() fields = %d, want 4", len(fields))
	}
	if fields[1] != "v9.9.9-test" {
		t.Errorf("field[1] = %q, want override %q used verbatim", fields[1], "v9.9.9-test")
	}
}

// TestVersionCmdRunPrintsExactlyOneLine verifies the version command's
// stdout output is exactly Version() plus a trailing newline -- no extra
// diagnostic output a downstream parser would need to filter.
func TestVersionCmdRunPrintsExactlyOneLine(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w
	var c VersionCmd
	runErr := c.Run()
	w.Close()
	os.Stdout = orig

	if runErr != nil {
		t.Fatalf("VersionCmd.Run() error = %v", runErr)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	want := Version() + "\n"
	if string(out) != want {
		t.Errorf("VersionCmd.Run() printed %q, want %q", out, want)
	}
}
