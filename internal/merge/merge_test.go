package merge

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/manifest"
	"github.com/danmestas/libfossil/internal/repo"
	"github.com/danmestas/libfossil/simio"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func hasFossil() bool {
	_, err := exec.LookPath("fossil")
	return err == nil
}

// createDivergentRepo creates a repo with:
//   - Initial checkin: file.txt = baseContent
//   - Branch A checkin: file.txt = localContent (child of initial)
//   - Branch B checkin: file.txt = remoteContent (child of initial)
//
// Returns the repo, initial rid, branch A rid, branch B rid.
func createDivergentRepo(t *testing.T, baseContent, localContent, remoteContent string) (*repo.Repo, libfossil.FslID, libfossil.FslID, libfossil.FslID) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}

	// Initial checkin.
	baseRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte(baseContent)}},
		Comment: "initial",
		User:    "testuser",
		Time:    time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("checkin base: %v", err)
	}

	// Branch A (local): child of initial.
	localRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte(localContent)}},
		Comment: "branch A",
		User:    "testuser",
		Parent:  baseRid,
		Time:    time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("checkin local: %v", err)
	}

	// Branch B (remote): also child of initial (creates a fork).
	remoteRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte(remoteContent)}},
		Comment: "branch B",
		User:    "testuser",
		Parent:  baseRid,
		Time:    time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("checkin remote: %v", err)
	}

	return r, baseRid, localRid, remoteRid
}

// expandFileFromCheckin reads file.txt content from a checkin manifest.
func expandFileFromCheckin(t *testing.T, r *repo.Repo, rid libfossil.FslID) []byte {
	t.Helper()
	files, err := manifest.ListFiles(r, rid)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	for _, f := range files {
		if f.Name == "file.txt" {
			frid, ok := blob.Exists(r.DB(), f.UUID)
			if !ok {
				t.Fatalf("blob %s not found", f.UUID)
			}
			data, err := content.Expand(r.DB(), frid)
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			return data
		}
	}
	t.Fatal("file.txt not found in checkin")
	return nil
}

func TestFindCommonAncestorLinear(t *testing.T) {
	r, baseRid, localRid, _ := createDivergentRepo(t, "base\n", "local\n", "remote\n")
	defer r.Close()

	ancestor, err := FindCommonAncestor(r, localRid, baseRid)
	if err != nil {
		t.Fatal(err)
	}
	// localRid's parent is baseRid, so ancestor should be baseRid.
	if ancestor != baseRid {
		t.Fatalf("ancestor=%d, want %d (base)", ancestor, baseRid)
	}
}

func TestFindCommonAncestorForked(t *testing.T) {
	r, baseRid, localRid, remoteRid := createDivergentRepo(t, "base\n", "local\n", "remote\n")
	defer r.Close()

	ancestor, err := FindCommonAncestor(r, localRid, remoteRid)
	if err != nil {
		t.Fatal(err)
	}
	if ancestor != baseRid {
		t.Fatalf("ancestor=%d, want %d (base)", ancestor, baseRid)
	}
}

func TestDetectForksFindsTwo(t *testing.T) {
	r, _, _, _ := createDivergentRepo(t, "base\n", "local\n", "remote\n")
	defer r.Close()

	forks, err := DetectForks(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(forks) == 0 {
		t.Fatal("expected at least one fork")
	}
	t.Logf("found %d fork(s): ancestor=%d local=%d remote=%d",
		len(forks), forks[0].Ancestor, forks[0].LocalTip, forks[0].RemoteTip)
}

func TestDetectForksLinear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "linear.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid1, _, _ := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("v1\n")}},
		Comment: "first",
		User:    "testuser",
		Time:    time.Now().UTC(),
	})
	manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("v2\n")}},
		Comment: "second",
		User:    "testuser",
		Parent:  rid1,
		Time:    time.Now().UTC(),
	})

	forks, err := DetectForks(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(forks) != 0 {
		t.Fatalf("expected no forks in linear history, got %d", len(forks))
	}
}

// --- Strategy tests with Fossil validation ---

func TestThreeWayStrategyWithFossilValidation(t *testing.T) {
	if !hasFossil() {
		t.Skip("fossil binary not found")
	}

	// Non-overlapping edits: local changes line 1, remote changes line 3.
	base := "line1\nline2\nline3\n"
	local := "LOCAL1\nline2\nline3\n"
	remote := "line1\nline2\nREMOTE3\n"

	r, baseRid, localRid, remoteRid := createDivergentRepo(t, base, local, remote)

	// Verify ancestor.
	ancestor, err := FindCommonAncestor(r, localRid, remoteRid)
	if err != nil {
		t.Fatal(err)
	}
	if ancestor != baseRid {
		t.Fatalf("ancestor=%d, want %d", ancestor, baseRid)
	}

	// Load content.
	baseContent := expandFileFromCheckin(t, r, baseRid)
	localContent := expandFileFromCheckin(t, r, localRid)
	remoteContent := expandFileFromCheckin(t, r, remoteRid)

	// Merge with three-way.
	strat := &ThreeWayText{}
	result, err := strat.Merge(baseContent, localContent, remoteContent)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Clean {
		t.Fatalf("expected clean merge, got %d conflicts", len(result.Conflicts))
	}
	expected := "LOCAL1\nline2\nREMOTE3\n"
	if string(result.Content) != expected {
		t.Fatalf("merged content = %q, want %q", result.Content, expected)
	}

	// Commit the merge result and verify with Fossil.
	mergeRid, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: result.Content}},
		Comment: "merge of A and B",
		User:    "testuser",
		Parent:  localRid,
		Time:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("checkin merge result: %v", err)
	}
	_ = mergeRid
	r.Close()

	// Fossil rebuild — validates repo integrity.
	cmd := exec.Command("fossil", "rebuild", r.Path())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild failed: %v\n%s", err, out)
	}
	t.Logf("fossil rebuild: %s", strings.TrimSpace(string(out)))

	// Fossil artifact — read the merged content back.
	r2, _ := repo.Open(r.Path())
	defer r2.Close()
	mergedContent := expandFileFromCheckin(t, r2, mergeRid)
	if string(mergedContent) != expected {
		t.Fatalf("fossil-validated content = %q, want %q", mergedContent, expected)
	}
	t.Log("three-way merge: Fossil validation PASSED")
}

func TestLastWriterWinsWithFossilValidation(t *testing.T) {
	if !hasFossil() {
		t.Skip("fossil binary not found")
	}

	base := "original content\n"
	local := "local edit\n"
	remote := "remote edit (newer)\n"

	r, _, localRid, _ := createDivergentRepo(t, base, local, remote)

	baseContent := []byte(base)
	localContent := []byte(local)
	remoteContent := []byte(remote)

	strat := &LastWriterWins{}
	result, err := strat.Merge(baseContent, localContent, remoteContent)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Clean {
		t.Fatal("last-writer-wins should always be clean")
	}
	// Remote wins (passed as remote).
	if string(result.Content) != "remote edit (newer)\n" {
		t.Fatalf("content = %q", result.Content)
	}

	// Commit and validate.
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: result.Content}},
		Comment: "last-writer-wins merge",
		User:    "testuser",
		Parent:  localRid,
		Time:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	r.Close()

	cmd := exec.Command("fossil", "rebuild", r.Path())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild: %v\n%s", err, out)
	}
	t.Logf("fossil rebuild: %s", strings.TrimSpace(string(out)))
	t.Log("last-writer-wins: Fossil validation PASSED")
}

func TestBinaryStrategyWithFossilValidation(t *testing.T) {
	if !hasFossil() {
		t.Skip("fossil binary not found")
	}

	base := "binary\x00base\xff"
	local := "binary\x00local\xff"
	remote := "binary\x00remote\xff"

	r, _, localRid, _ := createDivergentRepo(t, base, local, remote)

	strat := &Binary{}
	result, err := strat.Merge([]byte(base), []byte(local), []byte(remote))
	if err != nil {
		t.Fatal(err)
	}
	if result.Clean {
		t.Fatal("binary should always conflict")
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(result.Conflicts))
	}

	// User picks local version to resolve. Commit it.
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte(local)}},
		Comment: "binary conflict resolved (kept local)",
		User:    "testuser",
		Parent:  localRid,
		Time:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	r.Close()

	cmd := exec.Command("fossil", "rebuild", r.Path())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild: %v\n%s", err, out)
	}
	t.Logf("fossil rebuild: %s", strings.TrimSpace(string(out)))
	t.Log("binary strategy: Fossil validation PASSED")
}

func TestConflictForkWithFossilValidation(t *testing.T) {
	if !hasFossil() {
		t.Skip("fossil binary not found")
	}

	base := "config: v1\n"
	local := "config: v2-local\n"
	remote := "config: v2-remote\n"

	r, baseRid, localRid, remoteRid := createDivergentRepo(t, base, local, remote)

	strat := &ConflictFork{}
	result, err := strat.Merge([]byte(base), []byte(local), []byte(remote))
	if err != nil {
		t.Fatal(err)
	}
	if result.Clean {
		t.Fatal("conflict-fork should never be clean")
	}

	// Record in conflict table.
	if err := EnsureConflictTable(r); err != nil {
		t.Fatalf("EnsureConflictTable: %v", err)
	}
	if err := RecordConflictFork(r, "file.txt", int64(baseRid), int64(localRid), int64(remoteRid)); err != nil {
		t.Fatalf("RecordConflictFork: %v", err)
	}

	// Verify conflict table has the entry.
	forks, err := ListConflictForks(r)
	if err != nil {
		t.Fatalf("ListConflictForks: %v", err)
	}
	if len(forks) != 1 || forks[0] != "file.txt" {
		t.Fatalf("expected 1 fork for file.txt, got %v", forks)
	}

	// Resolve: user picks remote version.
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "file.txt", Content: []byte(remote)}},
		Comment: "conflict-fork resolved (picked remote)",
		User:    "testuser",
		Parent:  localRid,
		Time:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if err := ResolveConflictFork(r, "file.txt"); err != nil {
		t.Fatalf("ResolveConflictFork: %v", err)
	}

	// Verify fork is resolved.
	forks, _ = ListConflictForks(r)
	if len(forks) != 0 {
		t.Fatalf("expected 0 forks after resolve, got %v", forks)
	}

	r.Close()

	cmd := exec.Command("fossil", "rebuild", r.Path())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fossil rebuild: %v\n%s", err, out)
	}
	t.Logf("fossil rebuild: %s", strings.TrimSpace(string(out)))

	// Verify Fossil ignores the conflict table (it should rebuild fine).
	t.Log("conflict-fork: Fossil validation PASSED (conflict table ignored by fossil)")
}

func TestResolverPatternMatching(t *testing.T) {
	res := &Resolver{
		patterns: []PatternRule{
			{Glob: "*.yaml", Strategy: "last-writer-wins"},
			{Glob: "*.png", Strategy: "binary"},
			{Glob: "*.go", Strategy: "three-way"},
		},
		fallback: "three-way",
	}

	tests := []struct {
		file     string
		expected string
	}{
		{"config.yaml", "last-writer-wins"},
		{"image.png", "binary"},
		{"main.go", "three-way"},
		{"readme.txt", "three-way"}, // fallback
	}

	for _, tt := range tests {
		got := res.Resolve(tt.file)
		if got != tt.expected {
			t.Errorf("Resolve(%q) = %q, want %q", tt.file, got, tt.expected)
		}
	}
}
