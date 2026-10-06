package cli_test

// Tests for the CLI's merge and mark-resolved commands (#244), checked
// against what the fossil binary reads from the checkout afterwards.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/cli"
)

// branchedCheckout leaves a fossil checkout on trunk with a feat branch
// that changed a.txt alone and b.txt in conflict with trunk. feat is
// committed last, so it is the repository's newest checkin.
func branchedCheckout(t *testing.T) (bin, repoPath, ckDir string) {
	t.Helper()
	bin, repoPath, ckDir = fossilCheckout(t)
	seed := checkoutHash(t, bin, ckDir)
	writeCkFile(t, ckDir, "b.txt", "two trunk\n")
	runFossil(t, bin, ckDir, "commit", "-m", "trunk")
	runFossil(t, bin, ckDir, "update", seed)
	writeCkFile(t, ckDir, "a.txt", "one feat\n")
	writeCkFile(t, ckDir, "b.txt", "two feat\n")
	runFossil(t, bin, ckDir, "commit", "-m", "feat", "--branch", "feat")
	runFossil(t, bin, ckDir, "update", "trunk")
	return bin, repoPath, ckDir
}

// Merge merges into the checked-out version, not the repository's newest
// checkin, and records the merge the way fossil reads it.
func TestRepoMergeRecordsMergeForFossil(t *testing.T) {
	bin, repoPath, ckDir := branchedCheckout(t)

	cmd := &cli.RepoMergeCmd{Version: "feat", Dir: ckDir}
	if err := cmd.Run(&cli.Globals{Repo: repoPath}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	changes := fossilChanges(t, bin, ckDir)
	for _, want := range []string{"UPDATED_BY_MERGE a.txt", "CONFLICT", "MERGED_WITH"} {
		if !strings.Contains(changes, want) {
			t.Fatalf("fossil changes lacks %q:\n%s", want, changes)
		}
	}
	if got := readCkFile(t, ckDir, "a.txt"); got != "one feat\n" {
		t.Fatalf("a.txt = %q, want feat's content", got)
	}
}

// mark-resolved refuses a file that still holds conflict markers: the
// conflict ends when the markers are edited out, and fossil agrees. ci then
// commits the checkout with the merge parent.
func TestRepoMergeResolveAndCommit(t *testing.T) {
	bin, repoPath, ckDir := branchedCheckout(t)
	g := &cli.Globals{Repo: repoPath}
	if err := (&cli.RepoMergeCmd{Version: "feat", Dir: ckDir}).Run(g); err != nil {
		t.Fatalf("merge: %v", err)
	}

	err := (&cli.RepoMergeResolveCmd{File: "b.txt", Dir: ckDir}).Run(g)
	if err == nil || !strings.Contains(err.Error(), "still has conflict markers") {
		t.Fatalf("mark-resolved err = %v, want a refusal", err)
	}

	writeCkFile(t, ckDir, "b.txt", "two both\n")
	if changes := fossilChanges(t, bin, ckDir); strings.Contains(changes, "CONFLICT") {
		t.Fatalf("fossil still sees a conflict after the edit:\n%s", changes)
	}
	ci := &cli.RepoCiCmd{Message: "merge feat", User: "test", Dir: ckDir}
	if err := ci.Run(g); err != nil {
		t.Fatalf("ci: %v", err)
	}
	if info := runFossil(t, bin, ckDir, "info"); !strings.Contains(info, "merged-from:") {
		t.Fatalf("commit has no merge parent:\n%s", info)
	}
	if changes := fossilChanges(t, bin, ckDir); changes != "" {
		t.Fatalf("fossil changes after ci:\n%s", changes)
	}
}

// checkoutHash returns the hash of the checkin the checkout is on.
func checkoutHash(t *testing.T, bin, ckDir string) string {
	t.Helper()
	for _, line := range strings.Split(runFossil(t, bin, ckDir, "info"), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[0] == "checkout:" {
			return fields[1]
		}
	}
	t.Fatal("fossil info has no checkout line")
	return ""
}

// Merge run from inside the checkout, as the CLI defaults to (-d .), with a
// merged-in file under a directory: the checkout path is relative.
func TestRepoMergeFromInsideCheckout(t *testing.T) {
	bin, _, ckDir := fossilCheckout(t) // the repository is found from the checkout
	seed := checkoutHash(t, bin, ckDir)
	writeCkFile(t, ckDir, "b.txt", "two trunk\n")
	runFossil(t, bin, ckDir, "commit", "-m", "trunk")
	runFossil(t, bin, ckDir, "update", seed)
	if err := os.MkdirAll(filepath.Join(ckDir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCkFile(t, ckDir, "d/x.txt", "x\n")
	runFossil(t, bin, ckDir, "add", "d/x.txt")
	runFossil(t, bin, ckDir, "commit", "-m", "feat", "--branch", "feat")
	runFossil(t, bin, ckDir, "update", "trunk")

	t.Chdir(ckDir)
	if err := (&cli.RepoMergeCmd{Version: "feat", Dir: "."}).Run(&cli.Globals{}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	changes := fossilChanges(t, bin, ckDir)
	if !strings.Contains(changes, "ADDED_BY_MERGE d/x.txt") {
		t.Fatalf("fossil changes:\n%s", changes)
	}
}

// ci follows fossil's commit on what it will commit: a name it does not
// track is refused, as is a commit with nothing changed (unless
// --allow-empty), and a directory names the files under it.
func TestRepoCiSelectsLikeFossil(t *testing.T) {
	bin, repoPath, ckDir := fossilCheckout(t)
	g := &cli.Globals{Repo: repoPath}
	ci := func(files []string, allowEmpty bool) error {
		return (&cli.RepoCiCmd{
			Message: "m", User: "test", Dir: ckDir,
			Files: files, AllowEmpty: allowEmpty,
		}).Run(g)
	}

	if err := ci(nil, false); err == nil || !strings.Contains(err.Error(), "nothing") {
		t.Fatalf("ci with no changes: err = %v, want a refusal", err)
	}
	if err := ci([]string{filepath.Join(ckDir, "nope.txt")}, false); err == nil ||
		!strings.Contains(err.Error(), "not tracked") {
		t.Fatalf("ci of an unknown name: err = %v, want a refusal", err)
	}
	if err := ci([]string{filepath.Join(ckDir, "a.txt")}, false); err == nil {
		t.Fatal("ci of an unchanged file succeeded")
	}
	if err := ci(nil, true); err != nil {
		t.Fatalf("ci --allow-empty: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(ckDir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCkFile(t, ckDir, "d/x.txt", "x\n")
	runFossil(t, bin, ckDir, "add", "d/x.txt")
	writeCkFile(t, ckDir, "a.txt", "one edited\n")
	if err := ci([]string{filepath.Join(ckDir, "d")}, false); err != nil {
		t.Fatalf("ci of a directory: %v", err)
	}
	if got := fossilChanges(t, bin, ckDir); got != "EDITED     a.txt" {
		t.Fatalf("fossil changes after ci of d = %q, want only a.txt edited", got)
	}
}
