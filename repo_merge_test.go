package libfossil

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/branch"
	"github.com/danmestas/libfossil/internal/fsltype"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// makeBranch forks off parent into a new named branch and returns the
// branch-head rid (the branch-creation checkin itself).
func makeBranch(t *testing.T, r *Repo, parent int64, name string) int64 {
	t.Helper()
	rid, _, err := branch.Create(r.Inner(), branch.CreateOpts{
		Name:   name,
		Parent: fsltype.FslID(parent),
		User:   "test",
		Time:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("branch.Create %q: %v", name, err)
	}
	return int64(rid)
}

func TestBranchTip_Trunk(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "initial", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hi\n")},
	})

	tip, err := r.BranchTip("trunk")
	if err != nil {
		t.Fatalf("BranchTip trunk: %v", err)
	}
	if tip != base {
		t.Errorf("trunk tip = %d, want %d", tip, base)
	}

	follow := commit(t, r, base, "more", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hi more\n")},
	})
	tip2, err := r.BranchTip("trunk")
	if err != nil {
		t.Fatalf("BranchTip trunk (2nd): %v", err)
	}
	if tip2 != follow {
		t.Errorf("trunk tip after follow-up = %d, want %d", tip2, follow)
	}
}

func TestBranchTip_Feature(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "initial", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hi\n")},
	})
	branchHead := makeBranch(t, r, base, "feature")

	tip, err := r.BranchTip("feature")
	if err != nil {
		t.Fatalf("BranchTip feature: %v", err)
	}
	if tip != branchHead {
		t.Errorf("feature tip = %d, want %d", tip, branchHead)
	}

	follow := commit(t, r, branchHead, "feature work", []FileToCommit{
		{Name: "hello.txt", Content: []byte("feature hi\n")},
	})
	tip2, err := r.BranchTip("feature")
	if err != nil {
		t.Fatalf("BranchTip feature (2nd): %v", err)
	}
	if tip2 != follow {
		t.Errorf("feature tip after follow-up = %d, want %d", tip2, follow)
	}
}

func TestBranchTip_NotFound(t *testing.T) {
	r := newTestRepo(t)
	_ = commit(t, r, 0, "initial", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hi\n")},
	})
	if _, err := r.BranchTip("no-such-branch"); err == nil {
		t.Fatal("BranchTip: expected error for unknown branch, got nil")
	}
}

func TestMerge_Clean(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "initial", []FileToCommit{
		{Name: "a.txt", Content: []byte("base-a\n")},
		{Name: "b.txt", Content: []byte("base-b\n")},
	})
	branchHead := makeBranch(t, r, base, "feature")

	featureTip := commit(t, r, branchHead, "feature edits a", []FileToCommit{
		{Name: "a.txt", Content: []byte("feature-a\n")},
		{Name: "b.txt", Content: []byte("base-b\n")},
	})
	trunkTip := commit(t, r, base, "trunk edits b", []FileToCommit{
		{Name: "a.txt", Content: []byte("base-a\n")},
		{Name: "b.txt", Content: []byte("trunk-b\n")},
	})

	rid, uuid, err := r.Merge("feature", "trunk", "merge feature into trunk", "test")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if rid <= 0 {
		t.Fatalf("merge rid = %d, want > 0", rid)
	}
	if uuid == "" {
		t.Fatal("merge uuid is empty")
	}

	// Verify merged fileset contains both edits.
	files, err := r.ListFiles(rid)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Name] = f.UUID
	}
	if len(got) != 2 {
		t.Errorf("merged fileset has %d files, want 2 (%+v)", len(got), got)
	}
	aBytes := readBlob(t, r, rid, "a.txt")
	bBytes := readBlob(t, r, rid, "b.txt")
	if string(aBytes) != "feature-a\n" {
		t.Errorf("a.txt = %q, want %q", aBytes, "feature-a\n")
	}
	if string(bBytes) != "trunk-b\n" {
		t.Errorf("b.txt = %q, want %q", bBytes, "trunk-b\n")
	}

	// Verify plink rows: primary=trunkTip, secondary=featureTip.
	// CAST so the scan works on both drivers (ncruces returns BOOLEAN
	// columns as a bool-string that doesn't scan into int).
	rows, err := r.DB().Query("SELECT pid, CAST(isprim AS INTEGER) FROM plink WHERE cid=?", rid)
	if err != nil {
		t.Fatalf("plink query: %v", err)
	}
	defer rows.Close()
	seen := map[int64]int{}
	for rows.Next() {
		var pid int64
		var isprim int
		if err := rows.Scan(&pid, &isprim); err != nil {
			t.Fatalf("plink scan: %v", err)
		}
		seen[pid] = isprim
	}
	if len(seen) != 2 {
		t.Errorf("plink row count = %d, want 2 (seen=%v)", len(seen), seen)
	}
	if seen[trunkTip] != 1 {
		t.Errorf("primary parent (trunkTip=%d) isprim = %d, want 1", trunkTip, seen[trunkTip])
	}
	if seen[featureTip] != 0 {
		t.Errorf("merge parent (featureTip=%d) isprim = %d, want 0", featureTip, seen[featureTip])
	}
}

func TestMerge_Conflict(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "initial", []FileToCommit{
		{Name: "shared.txt", Content: []byte("line1\nline2\n")},
	})
	branchHead := makeBranch(t, r, base, "feature")
	_ = commit(t, r, branchHead, "feature edits line1", []FileToCommit{
		{Name: "shared.txt", Content: []byte("FEATURE\nline2\n")},
	})
	_ = commit(t, r, base, "trunk edits line1", []FileToCommit{
		{Name: "shared.txt", Content: []byte("TRUNK\nline2\n")},
	})

	_, _, err := r.Merge("feature", "trunk", "merge feature into trunk", "test")
	if err == nil {
		t.Fatal("Merge: expected conflict error, got nil")
	}
	if !errors.Is(err, ErrMergeConflict) {
		t.Errorf("err = %v, want errors.Is ErrMergeConflict", err)
	}
	var mce *MergeConflictError
	if !errors.As(err, &mce) {
		t.Fatalf("err = %v, want errors.As *MergeConflictError", err)
	}
	if len(mce.Files) != 1 || mce.Files[0] != "shared.txt" {
		t.Errorf("conflict files = %v, want [shared.txt]", mce.Files)
	}
}

func TestMerge_SrcBranchNotFound(t *testing.T) {
	r := newTestRepo(t)
	_ = commit(t, r, 0, "initial", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hi\n")},
	})
	_, _, err := r.Merge("nope", "trunk", "x", "test")
	if err == nil {
		t.Fatal("Merge: expected error for unknown src branch, got nil")
	}
	if !strings.Contains(err.Error(), "src branch") {
		t.Errorf("err = %v, want message mentioning src branch", err)
	}
}

func TestMerge_DstBranchNotFound(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "initial", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hi\n")},
	})
	_ = makeBranch(t, r, base, "feature")
	_, _, err := r.Merge("feature", "nope", "x", "test")
	if err == nil {
		t.Fatal("Merge: expected error for unknown dst branch, got nil")
	}
	if !strings.Contains(err.Error(), "dst branch") {
		t.Errorf("err = %v, want message mentioning dst branch", err)
	}
}

func TestMerge_SameBranch(t *testing.T) {
	r := newTestRepo(t)
	_ = commit(t, r, 0, "initial", []FileToCommit{
		{Name: "hello.txt", Content: []byte("hi\n")},
	})
	_, _, err := r.Merge("trunk", "trunk", "x", "test")
	if err == nil {
		t.Fatal("Merge: expected error for same-branch merge, got nil")
	}
}

func TestMerge_AddOnOneSide(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, 0, "initial", []FileToCommit{
		{Name: "a.txt", Content: []byte("shared\n")},
	})
	branchHead := makeBranch(t, r, base, "feature")
	_ = commit(t, r, branchHead, "feature adds b", []FileToCommit{
		{Name: "a.txt", Content: []byte("shared\n")},
		{Name: "b.txt", Content: []byte("new on feature\n")},
	})
	// trunk stays at base

	rid, _, err := r.Merge("feature", "trunk", "pick up b", "test")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	files, err := r.ListFiles(rid)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
	}
	if !names["a.txt"] || !names["b.txt"] {
		t.Errorf("merged fileset = %v, want a.txt and b.txt", names)
	}
	if got := string(readBlob(t, r, rid, "b.txt")); got != "new on feature\n" {
		t.Errorf("b.txt = %q, want %q", got, "new on feature\n")
	}
}

// readBlob returns the expanded bytes of filePath in the given checkin.
// Fails the test if the file is absent.
func readBlob(t *testing.T, r *Repo, rid int64, filePath string) []byte {
	t.Helper()
	data, err := blobAt(r, rid, filePath)
	if err != nil {
		t.Fatalf("blobAt %s in rid=%d: %v", filePath, rid, err)
	}
	if data == nil {
		t.Fatalf("%s not present in rid=%d", filePath, rid)
	}
	return data
}
