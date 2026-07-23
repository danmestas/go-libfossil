package cli_test

import (
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/danmestas/go-libfossil/cli"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// cloneTestCLI mirrors the wiring in cmd/libfossil/main.go closely enough to
// exercise kong flag parsing for RepoCloneCmd without depending on the main
// package.
type cloneTestCLI struct {
	cli.Globals
	Repo cli.RepoCmd `cmd:""`
}

// TestRepoCloneCmdDefaultTimeout pins the historical 10-minute deadline as the
// default so an ordinary clone behaves exactly as before the flag existed.
func TestRepoCloneCmdDefaultTimeout(t *testing.T) {
	var c cloneTestCLI
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"repo", "clone", "http://x", "out.fossil"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Repo.Clone.Timeout != 10*time.Minute {
		t.Errorf("default Timeout = %v, want 10m", c.Repo.Clone.Timeout)
	}
}

// TestRepoCloneCmdTimeoutFlagOverride raises the deadline past the default so a
// large repository can finish.
func TestRepoCloneCmdTimeoutFlagOverride(t *testing.T) {
	var c cloneTestCLI
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"repo", "clone", "--timeout", "45m", "http://x", "out.fossil"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Repo.Clone.Timeout != 45*time.Minute {
		t.Errorf("Timeout = %v, want 45m", c.Repo.Clone.Timeout)
	}
}

// TestRepoCloneCmdTimeoutDisable proves the deadline can be turned off entirely
// with 0 for an arbitrarily long clone.
func TestRepoCloneCmdTimeoutDisable(t *testing.T) {
	var c cloneTestCLI
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"repo", "clone", "--timeout", "0", "http://x", "out.fossil"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Repo.Clone.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (disabled)", c.Repo.Clone.Timeout)
	}
}
