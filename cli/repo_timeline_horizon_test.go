package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// In a structural store the parent of the horizon check-in is a boundary
// hash: the timeline shows it as the horizon, never as a missing parent
// (draft-fossil-horizon-00 §5.4).
func TestRepoTimelineRendersHorizon(t *testing.T) {
	url, c1, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "s.fossil")
	requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: url, Path: path, Horizon: c2, Structural: true}).Run(&Globals{})
	})
	var runErr error
	out := requiredHTTPStdout(t, func() error {
		runErr = (&RepoTimelineCmd{Limit: 20}).Run(&Globals{Repo: path})
		return nil
	})
	if runErr != nil {
		t.Fatalf("timeline: %v", runErr)
	}
	if want := "  parent: " + c1 + " (horizon)\n"; !strings.Contains(out, want) {
		t.Fatalf("timeline lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "missing") {
		t.Fatalf("timeline describes the horizon as damage:\n%s", out)
	}
	if strings.Count(out, "(horizon)") != 1 {
		t.Fatalf("timeline names the horizon more than once:\n%s", out)
	}
}

// A repository without a horizon prints no parent lines.
func TestRepoTimelineNoHorizonLines(t *testing.T) {
	url, _, _, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "plain.fossil")
	requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: url, Path: path}).Run(&Globals{})
	})
	out := requiredHTTPStdout(t, func() error { return (&RepoTimelineCmd{Limit: 20}).Run(&Globals{Repo: path}) })
	if strings.Contains(out, "parent:") {
		t.Fatalf("plain timeline prints parents:\n%s", out)
	}
}
