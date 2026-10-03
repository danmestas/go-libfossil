package cli_test

// Tests for the CLI's add, rm, rename and revert commands (#234). Each runs a
// command against a checkout the fossil binary created, then asks fossil
// itself what the checkout looks like, so a command that writes vfile in a
// way fossil reads differently fails here.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/cli"
	"github.com/danmestas/go-libfossil/testutil"
)

// fossilCheckout creates a repository and checkout with the fossil binary,
// with a.txt and b.txt committed, and returns the binary, repository path
// and checkout directory.
func fossilCheckout(t *testing.T) (bin, repoPath, ckDir string) {
	t.Helper()
	bin = testutil.RequireFossilBin(t)
	dir := t.TempDir()
	repoPath = filepath.Join(dir, "test.fossil")
	ckDir = filepath.Join(dir, "ck")
	if err := os.Mkdir(ckDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runFossil(t, bin, dir, "init", repoPath)
	runFossil(t, bin, ckDir, "open", repoPath)
	writeCkFile(t, ckDir, "a.txt", "one\n")
	writeCkFile(t, ckDir, "b.txt", "two\n")
	runFossil(t, bin, ckDir, "add", "a.txt", "b.txt")
	runFossil(t, bin, ckDir, "commit", "-m", "seed")
	return bin, repoPath, ckDir
}

func runFossil(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeCkFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCkFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fossilChanges(t *testing.T, bin, ckDir string) string {
	t.Helper()
	return strings.TrimSpace(runFossil(t, bin, ckDir, "changes"))
}

func TestRepoAddNewFile(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)
	writeCkFile(t, ckDir, "c.txt", "three\n")

	cmd := &cli.RepoAddCmd{Files: []string{filepath.Join(ckDir, "c.txt")}, Dir: ckDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := fossilChanges(t, bin, ckDir); got != "ADDED      c.txt" {
		t.Fatalf("fossil changes = %q, want c.txt ADDED", got)
	}
}

// Adding a file that is already tracked and unchanged must not mark it
// edited: fossil's add leaves it alone.
func TestRepoAddTrackedFileIsNoChange(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)

	cmd := &cli.RepoAddCmd{Files: []string{filepath.Join(ckDir, "a.txt")}, Dir: ckDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := fossilChanges(t, bin, ckDir); got != "" {
		t.Fatalf("fossil changes = %q after re-adding a tracked file, want none", got)
	}
}

func TestRepoRmMarksDeleted(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)

	cmd := &cli.RepoRmCmd{Files: []string{"a.txt"}, Dir: ckDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if got := fossilChanges(t, bin, ckDir); got != "DELETED    a.txt" {
		t.Fatalf("fossil changes = %q, want a.txt DELETED", got)
	}
}

// Rename moves the file on disk as well as in the checkout, like
// `fossil mv --hard`. Recording the new name while the file stays under the
// old one left the checkout tracking a file that is not there.
func TestRepoRenameMovesFile(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)

	cmd := &cli.RepoRenameCmd{From: "a.txt", To: "z.txt", Dir: ckDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := readCkFile(t, ckDir, "z.txt"); got != "one\n" {
		t.Fatalf("z.txt = %q, want a.txt's content", got)
	}
	if _, err := os.Stat(filepath.Join(ckDir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("a.txt still on disk after rename (stat err %v)", err)
	}
	got := fossilChanges(t, bin, ckDir)
	if !strings.Contains(got, "RENAMED") || !strings.Contains(got, "z.txt") {
		t.Fatalf("fossil changes = %q, want z.txt RENAMED", got)
	}
}

// Revert restores the committed content, as fossil's revert does, rather than
// only clearing the checkout's change flags over an edited file.
func TestRepoRevertRestoresContent(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)
	writeCkFile(t, ckDir, "a.txt", "edited\n")
	writeCkFile(t, ckDir, "b.txt", "edited too\n")

	one := &cli.RepoRevertCmd{Files: []string{"a.txt"}, Dir: ckDir}
	if err := one.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("revert a.txt: %v", err)
	}
	if got := readCkFile(t, ckDir, "a.txt"); got != "one\n" {
		t.Fatalf("a.txt = %q after revert, want the committed content", got)
	}

	all := &cli.RepoRevertCmd{Dir: ckDir}
	if err := all.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("revert all: %v", err)
	}
	if got := readCkFile(t, ckDir, "b.txt"); got != "two\n" {
		t.Fatalf("b.txt = %q after revert all, want the committed content", got)
	}
	if got := fossilChanges(t, bin, ckDir); got != "" {
		t.Fatalf("fossil changes = %q after revert, want none", got)
	}
}
