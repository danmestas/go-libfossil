package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain lets this test binary re-exec itself as the real libfossil CLI
// when invoked with LIBFOSSIL_TEST_MAIN=1: it calls main() directly and
// returns, without ever entering testing's own flag parsing or m.Run().
// runCLI uses this to observe the actual process exit status main()
// produces, which go test's own process does not expose.
func TestMain(m *testing.M) {
	if os.Getenv("LIBFOSSIL_TEST_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// runCLI runs this test binary as a subprocess re-entering main() via
// TestMain, with args passed through as if they were the real CLI's
// command line, and reports what that process printed and exited with.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "LIBFOSSIL_TEST_MAIN=1")

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	switch e := err.(type) {
	case nil:
		exitCode = 0
	case *exec.ExitError:
		exitCode = e.ExitCode()
	default:
		t.Fatalf("running subprocess: %v", err)
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// TestVersionCommandExitsZeroWithOneLine covers the core acceptance
// criterion: `libfossil version` exits 0 and prints exactly one
// non-empty, parseable line.
func TestVersionCommandExitsZeroWithOneLine(t *testing.T) {
	stdout, stderr, code := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr)
	}

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("stdout = %q, want exactly one non-empty line", stdout)
	}
	if !strings.HasPrefix(lines[0], "libfossil ") {
		t.Errorf("version line = %q, want prefix %q", lines[0], "libfossil ")
	}
}

// TestUnrecognizedCommandExitsNonZero locks in the exit-code half of the
// issue: a caller must be able to tell "the binary didn't understand what
// I asked" apart from "the binary answered".
func TestUnrecognizedCommandExitsNonZero(t *testing.T) {
	_, stderr, code := runCLI(t, "this-command-does-not-exist")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for an unrecognized command; stderr=%q", stderr)
	}
}

// TestUnrecognizedFlagExitsNonZero is the flag-parsing counterpart to
// TestUnrecognizedCommandExitsNonZero.
func TestUnrecognizedFlagExitsNonZero(t *testing.T) {
	_, stderr, code := runCLI(t, "--this-flag-does-not-exist")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for an unrecognized flag; stderr=%q", stderr)
	}
}

// TestVersionGlobalFlagExitsNonZero documents that --version is not a
// global flag: `repo extract` already owns --version (source version to
// extract) as command-scoped state, and a bare `--version` at the root is
// simply an unrecognized flag, same as any other -- it must fail loudly
// rather than silently succeed or silently fall back to help with exit 0.
func TestVersionGlobalFlagExitsNonZero(t *testing.T) {
	_, stderr, code := runCLI(t, "--version")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for the unrecognized global --version flag; stderr=%q", stderr)
	}
}

// TestNoArgsExitsNonZero covers running the bare binary with no command at
// all -- also a case the caller must be able to distinguish from success.
func TestNoArgsExitsNonZero(t *testing.T) {
	_, stderr, code := runCLI(t)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero when no command is given; stderr=%q", stderr)
	}
}
