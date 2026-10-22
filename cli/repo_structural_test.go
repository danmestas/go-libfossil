package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// A structural clone (draft-fossil-horizon-00 §5): --structural takes the
// structural cut and the clone says so; the store cone shows it; info names
// the horizon and the check-ins below it rather than leaving bare phantoms;
// verify counts the manifests below the horizon as elided, never as damage
// (§5.4); and checking out, diffing or annotating a check-in below C fails
// naming the horizon (§5.5).
func TestRepoCloneStructural(t *testing.T) {
	url, c1, c2, _ := horizonSource(t)
	path := filepath.Join(t.TempDir(), "s.fossil")
	if err := (&RepoCloneCmd{URL: url, Path: path, Structural: true}).Run(&Globals{}); err == nil {
		t.Fatal("--structural without --horizon was accepted")
	}
	out := requiredHTTPStdout(t, func() error {
		return (&RepoCloneCmd{URL: url, Path: path, Horizon: c2, Structural: true}).Run(&Globals{})
	})
	if !strings.Contains(out, "Horizon:      structural\n") {
		t.Fatalf("clone output = %q", out)
	}
	if got, want := storeConeOutput(t, path), "horizon "+c2+" descendants structural\n"; got != want {
		t.Fatalf("store cone = %q, want %q", got, want)
	}
	info := requiredHTTPStdout(t, func() error { return (&RepoInfoCmd{}).Run(&Globals{Repo: path}) })
	for _, line := range []string{"horizon:       " + c2 + " descendants structural\n", "below horizon: 1 check-in(s)\n"} {
		if !strings.Contains(info, line) {
			t.Fatalf("info lacks %q:\n%s", line, info)
		}
	}
	var verifyErr error
	verified := requiredHTTPStdout(t, func() error {
		verifyErr = (&RepoVerifyCmd{}).Run(&Globals{Repo: path})
		return nil
	})
	if verifyErr != nil || !strings.Contains(verified, "0 transit phantoms") ||
		!strings.Contains(verified, "1 check-in(s) below the horizon") {
		t.Fatalf("verify: %v\n%s", verifyErr, verified)
	}
	for name, run := range map[string]func() error{
		"co":       func() error { return (&RepoCoCmd{Version: c1, Dir: t.TempDir()}).Run(&Globals{Repo: path}) },
		"diff":     func() error { return (&RepoDiffCmd{Version: c1, To: c2}).Run(&Globals{Repo: path}) },
		"annotate": func() error { return (&RepoAnnotateCmd{File: "a", Version: c1}).Run(&Globals{Repo: path}) },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "below the structural horizon at "+c2) {
			t.Errorf("%s below C: %v, want a diagnostic naming the horizon", name, err)
		}
	}
}
