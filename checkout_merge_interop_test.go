package libfossil_test

// Interop tests for Checkout.Merge against the fossil binary (#244). Each
// test builds the same divergent history with fossil, then merges it with
// fossil and with go-libfossil, or merges with one and commits with the
// other, and checks that fossil reads the result the way it reads its own.

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// mergeFixtureFiles is every file the merge fixture touches.
var mergeFixtureFiles = []string{"a.txt", "c.txt", "d.txt", "e.txt", "new.txt", "x.sh"}

// newMergeFixture builds, with the fossil binary, a trunk and a feat branch
// that diverge in every way a merge handles, and leaves the checkout on
// trunk with nothing pending:
//
//	a.txt   changed on feat only           -> UPDATED_BY_MERGE
//	c.txt   deleted on feat                -> DELETED
//	d.txt   changed on both, other lines   -> clean merge (EDITED)
//	e.txt   changed on both, same line     -> CONFLICT
//	new.txt added on feat                  -> ADDED_BY_MERGE
//	x.sh    made executable on feat        -> executable bit taken
func newMergeFixture(t *testing.T) (bin, repoPath, ckDir string) {
	t.Helper()
	bin, repoPath, ckDir = newFossilCheckout(t)
	write := func(name, content string) {
		writeFile(t, filepath.Join(ckDir, name), content)
	}
	write("c.txt", "c\n")
	write("d.txt", "1\n2\n3\n4\n5\n")
	write("e.txt", "x\ny\nz\n")
	write("x.sh", "echo\n")
	fossilRun(t, bin, ckDir, "add", "c.txt", "d.txt", "e.txt", "x.sh")
	fossilRun(t, bin, ckDir, "commit", "-m", "base")

	write("a.txt", "one\nbranch\n")
	write("new.txt", "new\n")
	fossilRun(t, bin, ckDir, "add", "new.txt")
	fossilRun(t, bin, ckDir, "rm", "--hard", "c.txt")
	write("d.txt", "1b\n2\n3\n4\n5\n")
	write("e.txt", "x\nfeat\nz\n")
	if err := os.Chmod(filepath.Join(ckDir, "x.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	fossilRun(t, bin, ckDir, "commit", "-m", "feat", "--branch", "feat")

	fossilRun(t, bin, ckDir, "update", "trunk")
	write("d.txt", "1\n2\n3\n4\n5t\n")
	write("e.txt", "x\ntrunk\nz\n")
	fossilRun(t, bin, ckDir, "commit", "-m", "trunk")
	return bin, repoPath, ckDir
}

// openSecondCheckout opens another fossil checkout of repoPath, on trunk.
func openSecondCheckout(t *testing.T, bin, repoPath string) string {
	t.Helper()
	ckDir := filepath.Join(t.TempDir(), "ck2")
	if err := os.Mkdir(ckDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilRun(t, bin, ckDir, "open", repoPath, "trunk")
	return ckDir
}

// libfossilMerge merges branch into the checkout at ckDir with go-libfossil.
func libfossilMerge(
	t *testing.T, repoPath, ckDir, branch string,
) (libfossil.CheckoutMergeResult, *libfossil.Checkout) {
	t.Helper()
	ck := openLibfossilCheckout(t, repoPath, ckDir)
	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("close repo: %v", err)
		}
	}()
	tip, err := r.BranchTip(branch)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ck.Merge(libfossil.CheckoutMergeOpts{Version: tip})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return res, ck
}

// changeLines is `fossil changes` as sorted lines with the spacing
// normalized, MERGED_WITH included.
func changeLines(t *testing.T, bin, ckDir string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(fossilRun(t, bin, ckDir, "changes"), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			lines = append(lines, strings.Join(fields, " "))
		}
	}
	sort.Strings(lines)
	return lines
}

// readFixtureFile returns a file's content, prefixed with "+x " when it is
// executable, or "<absent>".
func readFixtureFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "<absent>"
	}
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o100 != 0 {
		return "+x " + string(data)
	}
	return string(data)
}

// TestMergeMatchesFossil merges the same branch with fossil and with
// go-libfossil in two checkouts and compares how fossil reads them: the
// same change classes, MERGED_WITH included, and the same file contents.
// The conflicted file is compared by its conflict state, since fossil adds
// line numbers and a suggested resolution to its markers.
func TestMergeMatchesFossil(t *testing.T) {
	bin, repoPath, fossilCk := newMergeFixture(t)
	libCk := openSecondCheckout(t, bin, repoPath)

	fossilRun(t, bin, fossilCk, "merge", "feat")
	res, ck := libfossilMerge(t, repoPath, libCk, "feat")

	got, want := changeLines(t, bin, libCk), changeLines(t, bin, fossilCk)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fossil changes after go-libfossil merge:\n%s\nafter fossil merge:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, name := range mergeFixtureFiles {
		if name == "e.txt" {
			continue
		}
		got, want := readFixtureFile(t, libCk, name), readFixtureFile(t, fossilCk, name)
		if got != want {
			t.Errorf("%s = %q, fossil wrote %q", name, got, want)
		}
	}

	var actions []string
	for _, f := range res.Files {
		actions = append(actions, f.Action+" "+f.Name)
	}
	wantActions := []string{
		"updated a.txt", "deleted c.txt", "merged d.txt", "conflict e.txt", "added new.txt",
		"mode x.sh",
	}
	if !reflect.DeepEqual(actions, wantActions) {
		t.Errorf("Merge files = %q, want %q", actions, wantActions)
	}
	conflicts, err := ck.Conflicts()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conflicts, []string{"e.txt"}) {
		t.Errorf("Conflicts = %q, want [e.txt]", conflicts)
	}
}

// TestConflictsReadsFossilMerge checks that Conflicts finds the markers
// fossil's own merge writes.
func TestConflictsReadsFossilMerge(t *testing.T) {
	bin, repoPath, ckDir := newMergeFixture(t)
	fossilRun(t, bin, ckDir, "merge", "feat")

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	conflicts, err := ck.Conflicts()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conflicts, []string{"e.txt"}) {
		t.Fatalf("Conflicts = %q, want [e.txt]", conflicts)
	}
}

// mergedFrom returns the merged-from line of `fossil info` for the
// checkout, or "".
func mergedFrom(t *testing.T, bin, ckDir string) string {
	t.Helper()
	for _, line := range strings.Split(fossilRun(t, bin, ckDir, "info"), "\n") {
		if strings.HasPrefix(line, "merged-from:") {
			return line
		}
	}
	return ""
}

// A go-libfossil merge committed by go-libfossil records the merged-in
// checkin as a merge parent, and leaves nothing pending.
func TestLibfossilMergeCommitRecordsParent(t *testing.T) {
	bin, repoPath, ckDir := newMergeFixture(t)
	_, ck := libfossilMerge(t, repoPath, ckDir, "feat")
	writeFile(t, filepath.Join(ckDir, "e.txt"), "x\nboth\nz\n")

	if _, _, err := ck.Checkin(libfossil.CheckoutCommitOpts{
		Message: "merge feat", User: "test",
	}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if mergedFrom(t, bin, ckDir) == "" {
		t.Fatalf("commit has no merge parent; fossil info:\n%s", fossilRun(t, bin, ckDir, "info"))
	}
	if got := changeLines(t, bin, ckDir); len(got) != 0 {
		t.Fatalf("fossil changes after commit = %q, want none", got)
	}
}

// Fossil commits a go-libfossil merge with the merge parent: it reads the
// vmerge row go-libfossil wrote.
func TestFossilCommitsLibfossilMerge(t *testing.T) {
	bin, repoPath, ckDir := newMergeFixture(t)
	libfossilMerge(t, repoPath, ckDir, "feat")
	writeFile(t, filepath.Join(ckDir, "e.txt"), "x\nboth\nz\n")

	fossilRun(t, bin, ckDir, "commit", "-m", "merge feat")
	if mergedFrom(t, bin, ckDir) == "" {
		t.Fatalf("fossil commit lost the merge parent; fossil info:\n%s",
			fossilRun(t, bin, ckDir, "info"))
	}
}

// Go-libfossil commits a fossil merge with the merge parent.
func TestLibfossilCommitsFossilMerge(t *testing.T) {
	bin, repoPath, ckDir := newMergeFixture(t)
	fossilRun(t, bin, ckDir, "merge", "feat")
	writeFile(t, filepath.Join(ckDir, "e.txt"), "x\nboth\nz\n")

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	if _, _, err := ck.Checkin(libfossil.CheckoutCommitOpts{
		Message: "merge feat", User: "test",
	}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if mergedFrom(t, bin, ckDir) == "" {
		t.Fatalf("commit has no merge parent; fossil info:\n%s", fossilRun(t, bin, ckDir, "info"))
	}
	if got := changeLines(t, bin, ckDir); len(got) != 0 {
		t.Fatalf("fossil changes after commit = %q, want none", got)
	}
}
