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
	libdb "github.com/danmestas/go-libfossil/db"
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
	// fossil's own scan would clear a stray chnged=1, so check the row
	// itself: re-adding must not mark the file edited.
	ckdb, err := libdb.OpenSQL(filepath.Join(ckDir, ".fslckout"), libdb.OpenConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ckdb.Close()
	var chnged int
	if err := ckdb.QueryRow(
		"SELECT chnged FROM vfile WHERE pathname='a.txt'",
	).Scan(&chnged); err != nil {
		t.Fatal(err)
	}
	if chnged != 0 {
		t.Fatalf("a.txt chnged = %d after re-adding, want 0", chnged)
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

// fossilCheckoutRelative is fossilCheckout with the checkout opened by a
// relative repository path, as `fossil open ../test.fossil` stores it.
func fossilCheckoutRelative(t *testing.T) (bin, ckDir string) {
	t.Helper()
	bin = testutil.RequireFossilBin(t)
	dir := t.TempDir()
	ckDir = filepath.Join(dir, "ck")
	if err := os.Mkdir(ckDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runFossil(t, bin, dir, "init", "test.fossil")
	runFossil(t, bin, ckDir, "open", "../test.fossil")
	writeCkFile(t, ckDir, "a.txt", "one\n")
	writeCkFile(t, ckDir, "b.txt", "two\n")
	runFossil(t, bin, ckDir, "add", "a.txt", "b.txt")
	runFossil(t, bin, ckDir, "commit", "-m", "seed")
	return bin, ckDir
}

// Without -R the commands find the repository the checkout records, even
// when fossil stored it as a path relative to the checkout and the command
// runs from another directory (#234 review).
func TestRepoRmWithoutRepoFlag(t *testing.T) {
	bin, ckDir := fossilCheckoutRelative(t)

	cmd := &cli.RepoRmCmd{Files: []string{"a.txt"}, Dir: ckDir}
	if err := cmd.Run(&cli.Globals{}); err != nil {
		t.Fatalf("rm without -R: %v", err)
	}
	if got := fossilChanges(t, bin, ckDir); got != "DELETED    a.txt" {
		t.Fatalf("fossil changes = %q, want a.txt DELETED", got)
	}
}

// Rename refuses to move onto a file already on disk, as fossil mv --hard
// does, and leaves everything as it was: that file may be the only copy.
func TestRepoRenameRefusesExistingTarget(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)
	writeCkFile(t, ckDir, "u.txt", "mine\n")

	cmd := &cli.RepoRenameCmd{From: "a.txt", To: "u.txt", Dir: ckDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err == nil {
		t.Fatal("rename onto an existing file succeeded")
	}
	if got := readCkFile(t, ckDir, "u.txt"); got != "mine\n" {
		t.Fatalf("u.txt = %q, the existing file was overwritten", got)
	}
	if got := readCkFile(t, ckDir, "a.txt"); got != "one\n" {
		t.Fatalf("a.txt = %q, want it untouched", got)
	}
	if got := fossilChanges(t, bin, ckDir); got != "" {
		t.Fatalf("fossil changes = %q after a refused rename, want none", got)
	}
}

// Revert restores a file that is missing from disk, named or not, as fossil
// does.
func TestRepoRevertRestoresMissingFile(t *testing.T) {
	_, repoPath, ckDir := fossilCheckout(t)
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.Remove(filepath.Join(ckDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	named := &cli.RepoRevertCmd{Files: []string{"a.txt"}, Dir: ckDir}
	if err := named.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("revert a.txt: %v", err)
	}
	all := &cli.RepoRevertCmd{Dir: ckDir}
	if err := all.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("revert all: %v", err)
	}
	if got := readCkFile(t, ckDir, "a.txt"); got != "one\n" {
		t.Fatalf("a.txt = %q, want it restored", got)
	}
	if got := readCkFile(t, ckDir, "b.txt"); got != "two\n" {
		t.Fatalf("b.txt = %q, want it restored", got)
	}
}

// Revert of everything undoes a pending merge completely: the merged file's
// content, its merged-in version, and the merge record, as fossil's revert
// does. Otherwise the next scan sees the file changed again and the next
// commit records a merge parent.
func TestRepoRevertDropsPendingMerge(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)
	writeCkFile(t, ckDir, "a.txt", "one\nbranch\n")
	runFossil(t, bin, ckDir, "commit", "-m", "branch", "--branch", "feat")
	runFossil(t, bin, ckDir, "update", "trunk")
	runFossil(t, bin, ckDir, "merge", "feat")

	cmd := &cli.RepoRevertCmd{Dir: ckDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got := readCkFile(t, ckDir, "a.txt"); got != "one\n" {
		t.Fatalf("a.txt = %q, want trunk's content", got)
	}
	if got := fossilChanges(t, bin, ckDir); got != "" {
		t.Fatalf("fossil changes = %q after revert, want none (merge dropped)", got)
	}
}

// A file a merge added is not in the checked-out version: reverting the merge
// removes it, as fossil does (DELETE), instead of keeping it tracked so the
// next commit would include it.
func TestRepoRevertRemovesFileAddedByMerge(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)
	runFossil(t, bin, ckDir, "branch", "new", "feat", "trunk")
	runFossil(t, bin, ckDir, "update", "feat")
	writeCkFile(t, ckDir, "new.txt", "from feat\n")
	runFossil(t, bin, ckDir, "add", "new.txt")
	runFossil(t, bin, ckDir, "commit", "-m", "add new.txt")
	runFossil(t, bin, ckDir, "update", "trunk")
	runFossil(t, bin, ckDir, "merge", "feat")

	cmd := &cli.RepoRevertCmd{Dir: ckDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ckDir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt still on disk after reverting the merge that added it (stat err %v)", err)
	}
	if got := fossilChanges(t, bin, ckDir); got != "" {
		t.Fatalf("fossil changes = %q after revert, want none", got)
	}
	if got := runFossil(t, bin, ckDir, "ls"); strings.Contains(got, "new.txt") {
		t.Fatalf("new.txt still tracked after revert:\n%s", got)
	}
}

// A renamed and edited file is not in the checked-out version under its new
// name, but it is a committed file: revert must never delete it. (Moving it
// back to its old name is #242.)
func TestRepoRevertKeepsRenamedEditedFile(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)
	runFossil(t, bin, ckDir, "mv", "--hard", "a.txt", "z.txt")
	writeCkFile(t, ckDir, "z.txt", "edited\n")

	cmd := &cli.RepoRevertCmd{Dir: ckDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	kept := false
	for _, name := range []string{"a.txt", "z.txt"} {
		data, err := os.ReadFile(filepath.Join(ckDir, name))
		if err == nil && string(data) == "one\n" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("revert deleted a renamed committed file instead of restoring it")
	}
}
