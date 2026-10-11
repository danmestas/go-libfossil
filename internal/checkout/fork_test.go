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

func TestWouldFork_TrunkNoSymTag(t *testing.T) {
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

	forked, err := co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if !forked {
		t.Fatal("WouldFork = false, want true (trunk fork without sym-trunk tag)")
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

func TestWouldFork_OpenSiblingLeaf(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, _ := forkTwoTrunkLeaves(t, r)

	forked, err := co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if !forked {
		t.Fatal("WouldFork = false, want true (two open leaves on trunk)")
	}
}

func TestWouldFork_ClosedSiblingLeaf(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()
	co, rid1 := forkTwoTrunkLeaves(t, r)

	// Close the sibling leaf (tag 9 is 'closed'); only the checkout's own
	// open leaf remains, so a commit would not fork.
	if _, err := r.DB().Exec(
		"INSERT INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) VALUES(9, 1, ?, ?, NULL, 0, ?)",
		int64(rid1), int64(rid1), int64(rid1)); err != nil {
		t.Fatalf("close sibling: %v", err)
	}

	forked, err := co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if forked {
		t.Fatal("WouldFork = true, want false (the only sibling leaf is closed)")
	}
}

func TestWouldFork_NonLeafParent(t *testing.T) {
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
	// so is no longer a leaf: a commit extends it rather than forking.
	if err := co.Extract(rid1, ExtractOpts{Force: true}); err != nil {
		t.Fatalf("Extract back: %v", err)
	}

	forked, err := co.WouldFork()
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if forked {
		t.Fatal("WouldFork = true, want false (current version is not a leaf)")
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
}
