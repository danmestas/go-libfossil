package libfossil_test

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil"
)

// updateFixture creates a repo, commits two revs via Repo.Commit, then
// creates a checkout (initialized to tip = rid2). Returns the second RID
// so callers can pass it as a deliberate Update target.
func updateFixture(t *testing.T) (*libfossil.Repo, *libfossil.Checkout, int64) {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "u.fossil")
	repo, err := libfossil.Create(repoPath, libfossil.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	// First commit (no parent — genesis).
	rid1, _, err := repo.Commit(libfossil.CommitOpts{
		Comment: "first", User: "test",
		Files: []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("v1")}},
	})
	if err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	// Second commit (parent = rid1). This becomes the tip.
	rid2, _, err := repo.Commit(libfossil.CommitOpts{
		Comment: "second", User: "test", ParentID: rid1,
		Files: []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("v2")}},
	})
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}

	// CreateCheckout (NOT OpenCheckout — the dir is fresh) initializes the
	// working tree to the tip checkin (rid2).
	checkoutDir := filepath.Join(dir, "wt")
	checkout, err := repo.CreateCheckout(checkoutDir, libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	return repo, checkout, rid2
}

func TestCheckoutUpdate_TargetRID(t *testing.T) {
	_, checkout, rid := updateFixture(t)
	if err := checkout.Update(libfossil.UpdateOpts{TargetRID: rid}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestCheckoutUpdate_ZeroTargetRIDIsTipUpdate(t *testing.T) {
	// TargetRID=0 means "update to current branch tip" per checkout package.
	_, checkout, _ := updateFixture(t)
	if err := checkout.Update(libfossil.UpdateOpts{TargetRID: 0}); err != nil {
		t.Fatalf("Update(0): %v", err)
	}
}

func TestCheckoutUpdate_NonexistentRIDErrors(t *testing.T) {
	_, checkout, _ := updateFixture(t)
	err := checkout.Update(libfossil.UpdateOpts{TargetRID: 999999})
	if err == nil {
		t.Fatal("expected error for missing RID, got nil")
	}
}
