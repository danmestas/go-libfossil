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

// TestAssertValidBuildVersionRejectsWhitespace is the regression test for
// the defect where an unsanitized -ldflags override broke Version()'s
// single-line, four-field contract: a space-containing value produced six
// fields instead of four, and a newline-containing value broke the
// single-line guarantee outright. Both must be rejected at the source
// (buildVersion, checked in init) rather than reaching Version() at all --
// see the comment on init for why this fails the whole binary rather than
// sanitizing or falling back.
func TestAssertValidBuildVersionRejectsWhitespace(t *testing.T) {
	cases := []struct {
		name string
		v    string
	}{
		{"space", "v1.0 with spaces"},
		{"newline", "v1.0\nwith a newline"},
		{"tab", "v1.0\twith a tab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("assertValidBuildVersion(%q) did not panic, want panic", tc.v)
				}
			}()
			assertValidBuildVersion(tc.v)
		})
	}
}

// TestAssertValidBuildVersionAcceptsCleanValues is the negative-space
// counterpart: values with no whitespace -- including empty, meaning no
// override was supplied -- must not panic.
func TestAssertValidBuildVersionAcceptsCleanValues(t *testing.T) {
	for _, v := range []string{"", "v0.6.3", "v0.6.3+a1b2c3d4e5f6"} {
		assertValidBuildVersion(v) // must not panic
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
