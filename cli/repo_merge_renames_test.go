package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/cli"
)

// merge prints what fossil's merge prints for a file the merged-in version
// renamed and both sides edited: the merge under the old name, the rename
// after it, and additions last.
func TestRepoMergePrintsRenamesLikeFossil(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)
	writeCkFile(t, ckDir, "a.txt", "one\nx\nx\nx\ntwo\n")
	runFossil(t, bin, ckDir, "commit", "-m", "base")
	runFossil(t, bin, ckDir, "mv", "--hard", "a.txt", "r.txt")
	writeCkFile(t, ckDir, "r.txt", "one feat\nx\nx\nx\ntwo\n")
	writeCkFile(t, ckDir, "n.txt", "new\n")
	runFossil(t, bin, ckDir, "add", "n.txt")
	runFossil(t, bin, ckDir, "commit", "-m", "feat", "--branch", "feat")
	runFossil(t, bin, ckDir, "update", "trunk")
	writeCkFile(t, ckDir, "a.txt", "one\nx\nx\nx\ntwo trunk\n")
	runFossil(t, bin, ckDir, "commit", "-m", "trunk")

	fossilCk := filepath.Join(t.TempDir(), "ck2")
	if err := os.Mkdir(fossilCk, 0o755); err != nil {
		t.Fatal(err)
	}
	runFossil(t, bin, fossilCk, "open", repoPath, "trunk")
	var want string
	for _, line := range strings.SplitAfter(runFossil(t, bin, fossilCk, "merge", "feat"), "\n") {
		if !strings.Contains(line, "fossil undo") {
			want += line
		}
	}

	got := captureStdout(t, func() error {
		return (&cli.RepoMergeCmd{Version: "feat", Dir: ckDir}).Run(&cli.Globals{Repo: repoPath})
	})
	if got != want {
		t.Errorf("merge printed:\n%s\nfossil merge printed:\n%s", got, want)
	}
	if got := readCkFile(t, ckDir, "r.txt"); got != "one feat\nx\nx\nx\ntwo trunk\n" {
		t.Errorf("r.txt = %q, want both edits", got)
	}
}
