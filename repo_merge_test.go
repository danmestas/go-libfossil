package libfossil

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/branch"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/tag"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
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
	data, err := blobAt(context.Background(), r, rid, filePath, nil)
	if err != nil {
		t.Fatalf("blobAt %s in rid=%d: %v", filePath, rid, err)
	}
	if data == nil {
		t.Fatalf("%s not present in rid=%d", filePath, rid)
	}
	return data
}

// renameMergeRepo commits base, f.txt alone, then feature from it and trunk
// from it with their own files (Files is the whole tree), and merges
// feature into trunk. It returns the merge's files, name to content, and
// the renames its manifest records, new name to old.
func renameMergeRepo(t *testing.T, feature, trunk []FileToCommit) (map[string]string, map[string]string) {
	t.Helper()
	r := newTestRepo(t)
	base := commit(t, r, 0, "base", []FileToCommit{{Name: "f.txt", Content: []byte("1\nx\nx\n2\n")}})
	branchHead := makeBranch(t, r, base, "feature")
	for _, c := range []struct {
		parent int64
		files  []FileToCommit
	}{{branchHead, feature}, {base, trunk}} {
		if _, _, err := r.Commit(CommitOpts{
			ParentID: c.parent, Files: c.files, Comment: "edit", User: "test", PartialManifest: true,
		}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	rid, _, err := r.Merge("feature", "trunk", "merge", "test")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	files, err := r.ListFiles(rid)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Name] = string(readBlob(t, r, rid, f.Name))
	}
	rows, err := r.DB().Query(`SELECT n.name, p.name FROM mlink
		JOIN filename n ON n.fnid=mlink.fnid JOIN filename p ON p.fnid=mlink.pfnid
		WHERE mid=? AND pfnid>0`, rid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	renames := map[string]string{}
	for rows.Next() {
		var to, from string
		if err := rows.Scan(&to, &from); err != nil {
			t.Fatal(err)
		}
		renames[to] = from
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got, renames
}

// Merge follows a rename the merged-in branch made, as fossil's merge does:
// the destination's edit lands under the new name, and the commit records
// the rename.
func TestMerge_FollowsRenameOnSource(t *testing.T) {
	got, renames := renameMergeRepo(t,
		[]FileToCommit{{Name: "r.txt", Content: []byte("1\nx\nx\n2\n"), OldName: "f.txt"}},
		[]FileToCommit{{Name: "f.txt", Content: []byte("1\nx\nx\n2t\n")}})
	if want := map[string]string{"r.txt": "1\nx\nx\n2t\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("merged files = %q, want %q", got, want)
	}
	if want := map[string]string{"r.txt": "f.txt"}; !reflect.DeepEqual(renames, want) {
		t.Errorf("merge renames = %q, want %q", renames, want)
	}
}

// A file the destination renamed keeps its name and takes the source's
// edit.
func TestMerge_FollowsRenameOnDestination(t *testing.T) {
	got, renames := renameMergeRepo(t,
		[]FileToCommit{{Name: "f.txt", Content: []byte("1f\nx\nx\n2\n")}},
		[]FileToCommit{{Name: "r.txt", Content: []byte("1\nx\nx\n2t\n"), OldName: "f.txt"}})
	if want := map[string]string{"r.txt": "1f\nx\nx\n2t\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("merged files = %q, want %q", got, want)
	}
	if len(renames) != 0 {
		t.Errorf("merge renames = %q, want none", renames)
	}
}

// A file the source deleted and the destination left alone is gone after
// the merge.
func TestMerge_DropsFileDeletedOnSource(t *testing.T) {
	got, _ := renameMergeRepo(t,
		[]FileToCommit{{Name: "g.txt", Content: []byte("g\n")}},
		[]FileToCommit{{Name: "f.txt", Content: []byte("1\nx\nx\n2\n")}, {Name: "h.txt", Content: []byte("h\n")}})
	if want := map[string]string{"g.txt": "g\n", "h.txt": "h\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("merged files = %q, want %q", got, want)
	}
}

// commitAt commits one file on parent at the given time.
func commitAt(t *testing.T, r *Repo, parent int64, comment string, at time.Time) int64 {
	t.Helper()
	rid, _, err := r.Commit(CommitOpts{
		ParentID: parent,
		Files:    []FileToCommit{{Name: "f", Content: []byte(comment + "\n")}},
		Comment:  comment,
		User:     "test",
		Time:     at,
	})
	if err != nil {
		t.Fatalf("Commit %q: %v", comment, err)
	}
	return rid
}

func TestCurrentTrunk_FollowsMainBranchSetting(t *testing.T) {
	r := newTestRepo(t)
	base := commitBranch(t, r, 0, "trunk", "base",
		[]FileToCommit{{Name: "f", Content: []byte("0\n")}})
	feat := makeBranch(t, r, base, "feat")

	got, err := r.CurrentTrunk()
	if err != nil {
		t.Fatalf("CurrentTrunk: %v", err)
	}
	if got != base {
		t.Errorf("CurrentTrunk with main-branch unset = %d, want %d", got, base)
	}
	if err := r.SetConfig("main-branch", "feat"); err != nil {
		t.Fatal(err)
	}
	if got, err = r.CurrentTrunk(); err != nil || got != feat {
		t.Errorf("CurrentTrunk with main-branch=feat = %d, %v; want %d", got, err, feat)
	}
}

func TestBranchOpenLeaf(t *testing.T) {
	r := newTestRepo(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := commitAt(t, r, 0, "base", t0)
	b1 := makeBranch(t, r, base, "feat")
	// newer is committed first, so it has the smaller rid; closed is the
	// newest of the three leaves but is closed.
	newer := commitAt(t, r, b1, "newer", time.Now().UTC().Add(2*time.Hour))
	older := commitAt(t, r, b1, "older", time.Now().UTC().Add(1*time.Hour))
	closed := commitAt(t, r, b1, "closed", time.Now().UTC().Add(3*time.Hour))
	_, err := tag.AddTag(r.Inner(), tag.TagOpts{
		TargetRID: fsltype.FslID(closed), TagName: "closed", TagType: tag.TagSingleton,
		User: "test", Time: time.Now().UTC().Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if older <= newer {
		t.Fatalf("setup: older rid %d should exceed newer rid %d", older, newer)
	}

	rid, open, err := r.BranchOpenLeaf("feat")
	if err != nil || !open || rid != newer {
		t.Errorf("BranchOpenLeaf(feat) = %d, %v, %v; want %d, true", rid, open, err, newer)
	}
	if _, _, err := r.BranchOpenLeaf(""); err == nil {
		t.Error("BranchOpenLeaf(\"\") succeeded, want an error")
	}
	if rid, open, err := r.BranchOpenLeaf("nosuch"); err != nil || open || rid != 0 {
		t.Errorf("BranchOpenLeaf(nosuch) = %d, %v, %v; want 0, false, nil", rid, open, err)
	}
}
