package testutil

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fossilConfigPaths lists the per-user databases fossil may create under home.
func fossilConfigPaths(home string) []string {
	return []string{
		filepath.Join(home, ".fossil"),
		filepath.Join(home, ".config", "fossil.db"),
	}
}

// TestIsolateFossilHomeKeepsGlobalConfigOut proves a fossil run after
// RequireFossilBin writes its global config under the per-test directory (#284).
// It does not compare the real files' mtimes: other processes on a shared
// machine legitimately touch them, which would make the test flaky.
func TestIsolateFossilHomeKeepsGlobalConfigOut(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no real home to compare against: %v", err)
	}
	bin := RequireFossilBin(t)
	home := os.Getenv("FOSSIL_HOME")
	if home == "" || home == realHome || os.Getenv("HOME") != home {
		t.Fatalf("HOME=%q FOSSIL_HOME=%q: want both set to one per-test dir, not %q",
			os.Getenv("HOME"), home, realHome)
	}
	if !strings.HasPrefix(os.Getenv("XDG_CONFIG_HOME"), home) {
		t.Fatalf("XDG_CONFIG_HOME=%q is outside the isolated home %q",
			os.Getenv("XDG_CONFIG_HOME"), home)
	}

	// `fossil new` records the repository in the global config.
	repoPath := filepath.Join(t.TempDir(), "iso.fossil")
	if out, err := exec.Command(bin, "new", repoPath).CombinedOutput(); err != nil {
		t.Fatalf("fossil new: %v\n%s", err, out)
	}
	found := false
	for _, p := range fossilConfigPaths(home) {
		if _, err := os.Stat(p); err == nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("fossil wrote no global config under %q; it is using another home", home)
	}
}

// fossilIsolationMarkers are the substrings that show a test file resolves
// fossil through the isolating helpers or sets its own private home.
var fossilIsolationMarkers = []string{
	"requirefossilbin(", "isolatefossilhome(", "newtestrepo(", "fossil_home=",
}

// notFossilExec lists test files whose exec.Command calls never run fossil.
var notFossilExec = map[string]string{
	"cmd/libfossil/main_test.go":                "re-executes the test binary",
	"cmd/libfossil/driver_dependencies_test.go": "runs the go tool",
	"testutil/server_unix.go":                   "runs the bin its caller resolved",
	"testutil/server_other.go":                  "runs the bin its caller resolved",
	"conformance/zit_interop_test.go":           "runs zit, under a HOME of its own",
	"internal/clearsign/exec.go":                "runs the repository's signing command and gpg",
	"internal/clearsign/clearsign_test.go":      "runs gpg, under the GNUPGHOME signtest sets",
	"internal/clearsign/signtest/signtest.go":   "runs ssh-keygen and gpg, under a GNUPGHOME of its own",
}

// TestFossilInvocationsIsolateHome fails when a Go file runs a subprocess
// without any means of isolating fossil's home. It is a static net for new
// tests: a test that shells out to fossil must resolve it through
// RequireFossilBin (or call IsolateFossilHome), so the developer's real
// ~/.fossil and ~/.config/fossil.db are never written (#284).
//
// The check is per file, not per function: one isolating marker anywhere in a
// file clears every exec.Command in it, so a file whose helpers call fossil
// directly still relies on its tests calling RequireFossilBin first. The
// markers match whole call names ("newtestrepo(" does not match
// NewTestRepoFromPath), so a helper that does not isolate does not count.
func TestFossilInvocationsIsolateHome(t *testing.T) {
	root := ".."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "node_modules" || n == "docs" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(src)
		if !strings.Contains(text, "exec.Command") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if _, ok := notFossilExec[rel]; ok {
			return nil
		}
		lower := strings.ToLower(text)
		for _, m := range fossilIsolationMarkers {
			if strings.Contains(lower, m) {
				return nil
			}
		}
		t.Errorf("%s runs exec.Command without testutil.RequireFossilBin or "+
			"IsolateFossilHome; fossil would write the real global config", rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
