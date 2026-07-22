package checkout

import (
	"testing"

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
	_, _, err = co.Commit(CommitOpts{Message: "second commit", User: "test"})
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
		Message: "branch commit",
		User:    "test",
		Branch:  "feature-x",
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
