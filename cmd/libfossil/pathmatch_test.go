package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/testutil"
)

// pathMatchCases are the spellings of one file, Notes/A.md, that fossil
// resolves differently depending on the case-sensitive setting and on
// normalization (file_tree_name: a leading ./, a trailing /, doubled and
// interior /./ separators).
var pathMatchCases = []string{
	"Notes/A.md",
	"notes/a.md",
	"NOTES/A.MD",
	"./Notes/A.md",
	"Notes//A.md",
	"Notes/./A.md",
	"Notes/A.md/",
	"Notes/",
	"notes",
	"./Notes",
	"Notes/B.md",
	"Note",
}

// pathMatchSetup builds a repository holding Notes/A.md at two versions and
// keep.md, sets its case-sensitive setting, and opens a checkout of it for
// fossil to resolve paths in the way fossil normalizes them.
func pathMatchSetup(t *testing.T, fossilBin, caseSensitive string) (repoPath, ckout, v1, v2 string) {
	t.Helper()
	repoPath = newSnapshotRepo(t)
	v1 = snapshot(t, repoPath, "Notes/A.md", "one\n")
	snapshot(t, repoPath, "keep.md", "same\n")
	v2 = snapshot(t, repoPath, "Notes/A.md", "two\n")

	ckout = filepath.Join(t.TempDir(), "ckout")
	if err := os.Mkdir(ckout, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"settings", "case-sensitive", caseSensitive, "-R", repoPath},
		{"open", repoPath},
	} {
		cmd := exec.Command(fossilBin, args...)
		cmd.Dir = ckout
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fossil %v: %v\n%s", args, err, out)
		}
	}
	// fossil diff accepts a PATH that matches nothing when a file of that
	// name exists on disk, and a case-insensitive filesystem makes every
	// spelling exist. Empty the checkout so only the repository decides.
	for _, name := range []string{"Notes", "keep.md"} {
		if err := os.RemoveAll(filepath.Join(ckout, name)); err != nil {
			t.Fatal(err)
		}
	}
	return repoPath, ckout, v1, v2
}

func fossilIn(dir, fossilBin string, args ...string) (string, error) {
	cmd := exec.Command(fossilBin, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func TestPathMatchingFollowsFossil(t *testing.T) {
	fossilBin := testutil.RequireFossilBin(t)

	for _, setting := range []string{"on", "off"} {
		t.Run("case-sensitive="+setting, func(t *testing.T) {
			repoPath, ckout, v1, v2 := pathMatchSetup(t, fossilBin, setting)

			for _, path := range pathMatchCases {
				// cat --at: fossil cat -r VERSION PATH
				want, ferr := fossilIn(ckout, fossilBin, "cat", "-r", v2, path)
				stdout, stderr, code := runCLI(t, "-R", repoPath, "repo", "cat", "--at", v2, path)
				if (ferr == nil) != (code == 0) || (ferr == nil && stdout != want) {
					t.Errorf("cat --at %q: exit %d stdout %q stderr %q; fossil err=%v stdout %q",
						path, code, stdout, stderr, ferr, want)
				}
				if ferr != nil && code != exitNotFound {
					t.Errorf("cat --at %q: exit %d, want %d (fossil exits 1; go-libfossil's contract is 2)", path, code, exitNotFound)
				}

				// diff A B PATH: fossil diff --from A --to B PATH
				fout, ferr := fossilIn(ckout, fossilBin, "diff", "--from", v1, "--to", v2, path)
				stdout, stderr, code = runCLI(t, "-R", repoPath, "repo", "diff", v1, v2, path)
				if (ferr == nil) != (code == 0) {
					t.Errorf("diff %q: exit %d stderr %q; fossil err=%v", path, code, stderr, ferr)
				} else if ferr == nil && strings.Contains(fout, "+two") != strings.Contains(stdout, "+two") {
					t.Errorf("diff %q: stdout %q; fossil stdout %q", path, stdout, fout)
				}
				if ferr != nil && code != exitNotFound {
					t.Errorf("diff %q: exit %d, want %d", path, code, exitNotFound)
				}

				// log --path: fossil finfo -b PATH (a file, never a directory)
				fout, ferr = fossilIn(ckout, fossilBin, "finfo", "-b", path)
				stdout, stderr, code = runCLI(t, "-R", repoPath, "repo", "log", "--path", path)
				if (ferr == nil) != (code == 0) {
					t.Errorf("log --path %q: exit %d stderr %q; fossil err=%v", path, code, stderr, ferr)
					continue
				}
				if ferr == nil {
					var want, got []string
					for _, l := range strings.Split(strings.TrimSpace(fout), "\n") {
						want = append(want, strings.Fields(l)[0])
					}
					for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
						got = append(got, strings.Fields(l)[0])
					}
					if strings.Join(got, " ") != strings.Join(want, " ") {
						t.Errorf("log --path %q = %v, fossil finfo = %v", path, got, want)
					}
				} else if code != exitNotFound {
					t.Errorf("log --path %q: exit %d, want %d", path, code, exitNotFound)
				}
			}
		})
	}
}

// TestLogMergesNamesThatDifferInCase: with the setting off, fossil finfo
// lists the history of every file name that matches, newest first.
func TestLogMergesNamesThatDifferInCase(t *testing.T) {
	fossilBin := testutil.RequireFossilBin(t)
	// The setup's Notes/A.md, at two versions, matches as well.
	repoPath, _, _, _ := pathMatchSetup(t, fossilBin, "off")
	lower := snapshot(t, repoPath, "notes/a.md", "lower\n")
	upper := snapshot(t, repoPath, "NOTES/A.md", "upper\n")

	rows := pathLog(t, repoPath, "--path", "Notes/a.MD")
	var got []string
	for _, r := range rows {
		got = append(got, r.UUID)
	}
	if len(got) != 4 || got[0] != upper || got[1] != lower {
		t.Errorf("log --path Notes/a.MD = %v, want 4 check-ins, [%s %s ...]", got, upper, lower)
	}
	if rows := pathLog(t, repoPath, "--path", "Notes/a.MD", "-n", "1"); len(rows) != 1 || rows[0].UUID != upper {
		t.Errorf("log -n 1 = %+v, want just %s", rows, upper)
	}
}

// TestPathsOutsideTheRepositoryAreNotFound pins the documented exit status
// for ../x and absolute paths: 2, where fossil refuses them with exit 1.
func TestPathsOutsideTheRepositoryAreNotFound(t *testing.T) {
	repoPath := newSnapshotRepo(t)
	v1 := snapshot(t, repoPath, "a.md", "one\n")
	v2 := snapshot(t, repoPath, "a.md", "two\n")
	for _, path := range []string{"../a.md", "/a.md", "../../etc/passwd"} {
		for _, args := range [][]string{
			{"repo", "cat", "--at", v2, path},
			{"repo", "log", "--path", path},
			{"repo", "diff", v1, v2, path},
		} {
			stdout, _, code := runCLI(t, append([]string{"-R", repoPath}, args...)...)
			if code != exitNotFound || stdout != "" {
				t.Errorf("%v: exit %d, stdout %q; want exit %d and no stdout", args, code, stdout, exitNotFound)
			}
		}
	}
}
