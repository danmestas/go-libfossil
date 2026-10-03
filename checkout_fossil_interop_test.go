package libfossil_test

// Interop tests for checkouts created by the canonical fossil binary
// (`fossil open`) and then driven through go-libfossil (#228).
//
// Fossil leaves vfile.mhash NULL unless a merge has given the file content
// other than its checked-out version (mrid != rid); the baseline hash of an
// ordinary file comes from blob.uuid for vfile.rid. These tests pin that
// go-libfossil reads such a checkout the way fossil does.

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/testutil"
)

// fossilRun runs the fossil binary in dir and fails the test on error.
func fossilRun(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newFossilCheckout creates a repo and a checkout of it with the fossil
// binary, commits a.txt and b.txt, and returns the fossil binary, the repo
// path, and the checkout directory.
func newFossilCheckout(t *testing.T) (bin, repoPath, ckDir string) {
	t.Helper()
	bin = testutil.RequireFossilBin(t)
	dir := t.TempDir()
	repoPath = filepath.Join(dir, "test.fossil")
	ckDir = filepath.Join(dir, "ck")
	if err := os.Mkdir(ckDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fossilRun(t, bin, dir, "init", repoPath)
	fossilRun(t, bin, ckDir, "open", repoPath)
	writeFile(t, filepath.Join(ckDir, "a.txt"), "one\n")
	writeFile(t, filepath.Join(ckDir, "b.txt"), "two\n")
	fossilRun(t, bin, ckDir, "add", "a.txt", "b.txt")
	fossilRun(t, bin, ckDir, "commit", "-m", "seed")
	return bin, repoPath, ckDir
}

// openLibfossilCheckout opens the fossil-made checkout through go-libfossil.
// The repo and checkout are closed at test cleanup.
func openLibfossilCheckout(t *testing.T, repoPath, ckDir string) *libfossil.Checkout {
	t.Helper()
	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("close repo: %v", err)
		}
	})
	ck, err := r.OpenCheckout(ckDir, libfossil.CheckoutOpenOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ck.Close(); err != nil {
			t.Errorf("close checkout: %v", err)
		}
	})
	return ck
}

func statusLines(t *testing.T, ck *libfossil.Checkout) []string {
	t.Helper()
	changes, err := ck.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var lines []string
	for _, c := range changes {
		lines = append(lines, c.Change+" "+c.Name)
	}
	sort.Strings(lines)
	return lines
}

func TestFossilCheckoutStatusClean(t *testing.T) {
	_, repoPath, ckDir := newFossilCheckout(t)
	ck := openLibfossilCheckout(t, repoPath, ckDir)

	if got := statusLines(t, ck); len(got) != 0 {
		t.Fatalf("clean fossil checkout: Status = %q, want none", got)
	}
}

func TestFossilCheckoutStatusReportsEdits(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	writeFile(t, filepath.Join(ckDir, "a.txt"), "one edited\n")
	writeFile(t, filepath.Join(ckDir, "c.txt"), "three\n")
	fossilRun(t, bin, ckDir, "add", "c.txt")

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	got := strings.Join(statusLines(t, ck), ",")
	if want := "added c.txt,modified a.txt"; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
}

// A merge leaves vfile.mhash set and chnged=2 (UPDATED_BY_MERGE). Scanning
// must judge the file against the merged-in hash, report it, and leave
// fossil's merge state intact.
func TestFossilCheckoutStatusDuringMerge(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	writeFile(t, filepath.Join(ckDir, "a.txt"), "one\nbranch\n")
	fossilRun(t, bin, ckDir, "commit", "-m", "branch", "--branch", "feat")
	fossilRun(t, bin, ckDir, "update", "trunk")
	fossilRun(t, bin, ckDir, "merge", "feat")

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	got := strings.Join(statusLines(t, ck), ",")
	if want := "modified a.txt"; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
	changes := fossilRun(t, bin, ckDir, "changes")
	if !strings.Contains(changes, "UPDATED_BY_MERGE a.txt") {
		t.Fatalf("merge state lost after Status; fossil changes:\n%s", changes)
	}
}

// A non-forced Extract must refuse to clobber a local edit in a fossil-made
// checkout, the same as it does in one go-libfossil created.
func TestFossilCheckoutExtractKeepsLocalEdit(t *testing.T) {
	_, repoPath, ckDir := newFossilCheckout(t)
	aPath := filepath.Join(ckDir, "a.txt")
	writeFile(t, aPath, "local edit\n")

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	rid, _, err := ck.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := ck.Extract(rid, libfossil.ExtractOpts{}); err == nil {
		t.Fatal("Extract over a local edit succeeded, want a local-changes error")
	}
	data, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "local edit\n" {
		t.Fatalf("a.txt = %q, local edit was overwritten", data)
	}
}

// A non-forced Extract must not overwrite the result of a merge that fossil
// has applied but not yet committed: the merged file has no local "edit"
// relative to the merged-in artifact, but extracting the checked-out version
// over it would silently drop the merge.
func TestFossilCheckoutExtractKeepsPendingMerge(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	aPath := filepath.Join(ckDir, "a.txt")
	writeFile(t, aPath, "one\nbranch\n")
	fossilRun(t, bin, ckDir, "commit", "-m", "branch", "--branch", "feat")
	fossilRun(t, bin, ckDir, "update", "trunk")
	fossilRun(t, bin, ckDir, "merge", "feat")

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	rid, _, err := ck.Version()
	if err != nil {
		t.Fatal(err)
	}
	if err := ck.Extract(rid, libfossil.ExtractOpts{}); err == nil {
		t.Fatal("Extract over a pending merge succeeded, want a pending-merge error")
	}
	data, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\nbranch\n" {
		t.Fatalf("a.txt = %q, merge result was overwritten", data)
	}
}

// `fossil mv` records a rename in origname without touching chnged, so
// HasChanges must see renames through origname, as Status does.
func TestFossilCheckoutRename(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	fossilRun(t, bin, ckDir, "mv", "--hard", "a.txt", "z.txt")

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	has, err := ck.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("HasChanges = false after fossil mv, want true")
	}
	got := strings.Join(statusLines(t, ck), ",")
	if want := "renamed z.txt"; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
}

// Both tools working one checkout: a file added through go-libfossil must
// read back cleanly in go-libfossil and show as ADDED in fossil. Fossil's
// vfile schema gives islink no default, so a writer that omits it leaves a
// NULL that broke Status.
func TestFossilCheckoutLibfossilAdd(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	writeFile(t, filepath.Join(ckDir, "d.txt"), "four\n")

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	added, err := ck.Add([]string{"d.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("Add = %d, want 1", added)
	}
	got := strings.Join(statusLines(t, ck), ",")
	if want := "added d.txt"; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}

	changes := fossilRun(t, bin, ckDir, "changes")
	if !strings.Contains(changes, "ADDED      d.txt") {
		t.Fatalf("fossil changes does not show the add:\n%s", changes)
	}
}

// A version whose content is partly missing (a phantom, as a partial sync
// leaves it) is refused for checkout by fossil and by go-libfossil alike
// (#231). The test commits c.txt with fossil, turns its blob into a phantom,
// then asks both tools to check that version out into a fresh directory.
func TestMissingContentRefusedLikeFossil(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	writeFile(t, filepath.Join(ckDir, "c.txt"), "three\n")
	fossilRun(t, bin, ckDir, "add", "c.txt")
	fossilRun(t, bin, ckDir, "commit", "-m", "add c")
	fossilRun(t, bin, ckDir, "close", "--force")
	fossilRun(t, bin, filepath.Dir(repoPath), "sql", "-R", repoPath,
		"UPDATE blob SET size=-1, content=NULL WHERE rid=(SELECT fid FROM mlink "+
			"JOIN filename USING(fnid) WHERE name='c.txt'); "+
			"INSERT OR IGNORE INTO phantom(rid) SELECT rid FROM blob WHERE size<0;")

	fossilDir := t.TempDir()
	cmd := exec.Command(bin, "open", repoPath, "--workdir", fossilDir)
	cmd.Dir = fossilDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("fossil open succeeded on missing content:\n%s", out)
	}
	if !strings.Contains(string(out), "missing content") {
		t.Fatalf("fossil open failed for another reason:\n%s", out)
	}

	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ck, err := r.CreateCheckout(t.TempDir(), libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	defer ck.Close()
	tip, err := r.ResolveVersion("trunk")
	if err != nil {
		t.Fatal(err)
	}
	err = ck.Extract(tip, libfossil.ExtractOpts{})
	if err == nil {
		t.Fatal("Extract of a version with missing content succeeded")
	}
	if !strings.Contains(err.Error(), "content missing") {
		t.Fatalf("Extract should refuse for missing content, got: %v", err)
	}
	if !strings.Contains(err.Error(), "c.txt") {
		t.Fatalf("Extract error should name c.txt, got: %v", err)
	}
}

// A tracked file gone from disk is reported as missing, as fossil's changes
// command reports MISSING (#232): both a committed file deleted outside
// fossil and a fossil add whose file was then deleted.
func TestFossilCheckoutStatusReportsMissing(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	writeFile(t, filepath.Join(ckDir, "c.txt"), "three\n")
	fossilRun(t, bin, ckDir, "add", "c.txt")
	for _, name := range []string{"a.txt", "c.txt"} {
		if err := os.Remove(filepath.Join(ckDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	changes := fossilRun(t, bin, ckDir, "changes")
	for _, want := range []string{"MISSING    a.txt", "MISSING    c.txt"} {
		if !strings.Contains(changes, want) {
			t.Fatalf("fossil changes lacks %q:\n%s", want, changes)
		}
	}

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	got := strings.Join(statusLines(t, ck), ",")
	if want := "missing a.txt,missing c.txt"; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
}

// HasChanges sees a fossil add without a scan: fossil records the add as
// rid=0 with chnged left 0, and rid=0 means added (#232).
func TestFossilCheckoutHasChangesSeesAddWithoutScan(t *testing.T) {
	bin, repoPath, ckDir := newFossilCheckout(t)
	writeFile(t, filepath.Join(ckDir, "c.txt"), "three\n")
	fossilRun(t, bin, ckDir, "add", "c.txt")

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	has, err := ck.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("HasChanges = false with a pending fossil add, want true")
	}
}

// A directory where a tracked file belongs is not a file: fossil's changes
// reports it as NOT_A_FILE, under its missing filter. Status reports it as
// missing instead of failing (#232).
func TestFossilCheckoutStatusDirectoryInPlaceOfFile(t *testing.T) {
	_, repoPath, ckDir := newFossilCheckout(t)
	aPath := filepath.Join(ckDir, "a.txt")
	if err := os.Remove(aPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(aPath, 0o755); err != nil {
		t.Fatal(err)
	}

	ck := openLibfossilCheckout(t, repoPath, ckDir)
	got := strings.Join(statusLines(t, ck), ",")
	if want := "missing a.txt"; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
}
