package checkout

import (
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestBranchLeaves_SingleLeaf(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	leaves, err := BranchLeaves(r, "trunk")
	if err != nil {
		t.Fatalf("BranchLeaves: %v", err)
	}
	if len(leaves) != 1 {
		t.Fatalf("got %d leaves, want 1", len(leaves))
	}
}

func TestBranchLeaves_Empty(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	leaves, err := BranchLeaves(r, "nonexistent-branch")
	if err != nil {
		t.Fatalf("BranchLeaves: %v", err)
	}
	if len(leaves) != 0 {
		t.Fatalf("got %d leaves, want 0", len(leaves))
	}
}

func TestWouldFork_SingleLeaf(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer co.Close()

	forked, err := co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if forked {
		t.Fatal("WouldFork = true, want false (single leaf)")
	}
}

func TestLeafAmbiguous_TrunkNoSymTag(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer co.Close()

	rid1, _, _ := co.Version()

	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	co.env.Storage.WriteFile(co.dir+"/fork.txt", []byte("fork"), 0644)
	_, _, err = co.Commit(CommitOpts{Message: "second commit", User: "test", AllowEmpty: true})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Manually re-insert the original commit as a leaf to simulate a fork.
	_, err = r.DB().Exec("INSERT OR IGNORE INTO leaf(rid) VALUES(?)", int64(rid1))
	if err != nil {
		t.Fatalf("insert leaf: %v", err)
	}

	rid2, _, _ := co.Version()
	ambiguous, err := LeafAmbiguous(r, rid2)
	if err != nil {
		t.Fatalf("LeafAmbiguous: %v", err)
	}
	if !ambiguous {
		t.Fatal("LeafAmbiguous = false, want true (trunk fork without sym-trunk tag)")
	}
}

// forkTwoTrunkLeaves commits a child on trunk and re-inserts the original
// check-in as a leaf, so the checkout sits on an open leaf (the child) with a
// sibling open leaf (the original) on the same branch.
func forkTwoTrunkLeaves(t *testing.T, r *repo.Repo) (*Checkout, libfossil.FslID) {
	t.Helper()
	co, err := Create(r, t.TempDir(), CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { co.Close() })
	rid1, _, _ := co.Version()
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	co.env.Storage.WriteFile(co.dir+"/fork.txt", []byte("fork"), 0644)
	if _, _, err := co.Commit(CommitOpts{Message: "second", User: "test", AllowEmpty: true}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := r.DB().Exec("INSERT OR IGNORE INTO leaf(rid) VALUES(?)", int64(rid1)); err != nil {
		t.Fatalf("insert leaf: %v", err)
	}
	return co, rid1
}

// An open sibling leaf makes the checkout's leaf ambiguous, but a commit on
// it extends that leaf, so fossil does not refuse it as a fork.
func TestLeafAmbiguous_OpenSiblingLeaf(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, _ := forkTwoTrunkLeaves(t, r)
	rid2, _, _ := co.Version()

	ambiguous, err := LeafAmbiguous(r, rid2)
	if err != nil {
		t.Fatalf("LeafAmbiguous: %v", err)
	}
	if !ambiguous {
		t.Fatal("LeafAmbiguous = false, want true (two open leaves on trunk)")
	}
	forked, err := co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if forked {
		t.Fatal("WouldFork = true, want false (the version is a leaf)")
	}
}

func TestLeafAmbiguous_ClosedSiblingLeaf(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, rid1 := forkTwoTrunkLeaves(t, r)

	// Close the sibling leaf (tag 9 is 'closed'); only the checkout's own
	// open leaf remains, so the leaf is not ambiguous.
	if _, err := r.DB().Exec(
		"INSERT INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) VALUES(9, 1, ?, ?, NULL, 0, ?)",
		int64(rid1), int64(rid1), int64(rid1)); err != nil {
		t.Fatalf("close sibling: %v", err)
	}

	rid2, _, _ := co.Version()
	ambiguous, err := LeafAmbiguous(r, rid2)
	if err != nil {
		t.Fatalf("LeafAmbiguous: %v", err)
	}
	if ambiguous {
		t.Fatal("LeafAmbiguous = true, want false (the only sibling leaf is closed)")
	}
}

func TestWouldFork_NonLeafVersion(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, _ := Create(r, t.TempDir(), CreateOpts{})
	defer co.Close()

	rid1, _, _ := co.Version()
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	co.env.Storage.WriteFile(co.dir+"/child.txt", []byte("child"), 0644)
	if _, _, err := co.Commit(CommitOpts{Message: "child", User: "test", AllowEmpty: true}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Move the checkout back onto rid1, which now has a same-branch child and
	// so is no longer a leaf: a commit on it would be a second child, a fork.
	// This is also the state after a pull brings in a child of the checkout.
	if err := co.Extract(rid1, ExtractOpts{Force: true}); err != nil {
		t.Fatalf("Extract back: %v", err)
	}

	forked, err := co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if !forked {
		t.Fatal("WouldFork = false, want true (current version is not a leaf)")
	}
	ambiguous, err := LeafAmbiguous(r, rid1)
	if err != nil {
		t.Fatalf("LeafAmbiguous: %v", err)
	}
	if ambiguous {
		t.Fatal("LeafAmbiguous = true, want false (current version is not a leaf)")
	}
}

func TestWouldFork_DifferentBranch(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer co.Close()

	rid1, _, _ := co.Version()
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	co.env.Storage.WriteFile(co.dir+"/b.txt", []byte("branch"), 0644)

	_, _, err = co.Commit(CommitOpts{
		Message:    "branch commit",
		User:       "test",
		Branch:     "feature-x",
		AllowEmpty: true,
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	_, err = r.DB().Exec("INSERT OR IGNORE INTO leaf(rid) VALUES(?)", int64(rid1))
	if err != nil {
		t.Fatalf("insert leaf: %v", err)
	}

	forked, err := co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if forked {
		t.Fatal("WouldFork = true, want false (other leaf is on different branch)")
	}

	// Back on rid1, whose only child is on feature-x: rid1 is still a trunk
	// leaf, so a trunk commit on it does not fork.
	if err := co.Extract(rid1, ExtractOpts{Force: true}); err != nil {
		t.Fatalf("Extract back: %v", err)
	}
	forked, err = co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork on rid1: %v", err)
	}
	if forked {
		t.Fatal("WouldFork on rid1 = true, want false (its child is on another branch)")
	}
}

// fossil's is_a_leaf counts every plink child, merge children included, so a
// version merged into a same-branch check-in is no longer a leaf. The test
// demotes the child's plink row to a merge link (isprim=0) and expects the
// fork check to hold.
func TestWouldFork_MergeChild(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, _ := Create(r, t.TempDir(), CreateOpts{})
	defer co.Close()

	rid1, _, _ := co.Version()
	if err := co.Extract(rid1, ExtractOpts{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	co.env.Storage.WriteFile(co.dir+"/child.txt", []byte("child"), 0644)
	commit := CommitOpts{Message: "child", User: "test", AllowEmpty: true}
	if _, _, err := co.Commit(commit); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := r.DB().Exec("UPDATE plink SET isprim = 0 WHERE pid = ?", int64(rid1)); err != nil {
		t.Fatalf("demote plink: %v", err)
	}
	if err := co.Extract(rid1, ExtractOpts{Force: true}); err != nil {
		t.Fatalf("Extract back: %v", err)
	}

	forked, err := co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if !forked {
		t.Fatal("WouldFork = false, want true (a merge child makes the version a non-leaf)")
	}
}
