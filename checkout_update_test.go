package libfossil_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil"
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
	result, err := checkout.Update(libfossil.UpdateOpts{TargetRID: rid})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(result.Conflicted) != 0 {
		t.Fatalf("expected clean update, got Conflicted=%v", result.Conflicted)
	}
}

func TestCheckoutUpdate_ZeroTargetRIDIsTipUpdate(t *testing.T) {
	// TargetRID=0 means "update to current branch tip" per checkout package.
	_, checkout, _ := updateFixture(t)
	result, err := checkout.Update(libfossil.UpdateOpts{TargetRID: 0})
	if err != nil {
		t.Fatalf("Update(0): %v", err)
	}
	if len(result.Conflicted) != 0 {
		t.Fatalf("expected clean update, got Conflicted=%v", result.Conflicted)
	}
}

func TestCheckoutUpdate_NonexistentRIDErrors(t *testing.T) {
	_, checkout, _ := updateFixture(t)
	result, err := checkout.Update(libfossil.UpdateOpts{TargetRID: 999999})
	if err == nil {
		t.Fatal("expected error for missing RID, got nil")
	}
	// A genuine failure must be distinguishable from a conflicted success:
	// no marker paths are reported for a failed update.
	if len(result.Conflicted) != 0 {
		t.Fatalf("expected empty Conflicted on failure, got %v", result.Conflicted)
	}
}

// TestCheckoutUpdate_CleanReportsWrittenPaths verifies the clean-success
// outcome: err == nil, Conflicted empty, and the modified file's path shows
// up in FilesWritten (not just a count). updateFixture's checkout starts at
// tip (rid2), so this pins the checkout back to rid1 first via Extract to
// force Update(TargetRID: rid2) to do real work.
func TestCheckoutUpdate_CleanReportsWrittenPaths(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "clean.fossil")
	repo, err := libfossil.Create(repoPath, libfossil.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	rid1, _, err := repo.Commit(libfossil.CommitOpts{
		Comment: "first", User: "test",
		Files: []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("v1")}},
	})
	if err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	rid2, _, err := repo.Commit(libfossil.CommitOpts{
		Comment: "second", User: "test", ParentID: rid1,
		Files: []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("v2")}},
	})
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}

	checkoutDir := filepath.Join(dir, "wt")
	checkout, err := repo.CreateCheckout(checkoutDir, libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	t.Cleanup(func() { _ = checkout.Close() })

	// Pin the checkout to rid1 so Update(TargetRID: rid2) has real work to do.
	if err := checkout.Extract(rid1, libfossil.ExtractOpts{Force: true}); err != nil {
		t.Fatalf("Extract rid1: %v", err)
	}

	result, err := checkout.Update(libfossil.UpdateOpts{TargetRID: rid2})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(result.Conflicted) != 0 {
		t.Fatalf("expected clean update, got Conflicted=%v", result.Conflicted)
	}
	found := false
	for _, p := range result.FilesWritten {
		if p == "a.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected FilesWritten to contain a.txt, got %v", result.FilesWritten)
	}
}

// TestCheckoutUpdate_ConflictedSurfacesPaths builds a genuine fork at the
// public API level — two commits from the same parent, both editing the
// same line — and updates across it. The merge cannot resolve the
// overlapping edit, so it writes conflict markers into the working file.
// This is a SUCCESSFUL update (err == nil): the caller must be able to see
// which paths now contain markers via Conflicted.
func TestCheckoutUpdate_ConflictedSurfacesPaths(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "c.fossil")
	repo, err := libfossil.Create(repoPath, libfossil.CreateOpts{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	ridBase, _, err := repo.Commit(libfossil.CommitOpts{
		Comment: "base", User: "test",
		Files: []libfossil.FileToCommit{{Name: "conflict.txt", Content: []byte("original\n")}},
	})
	if err != nil {
		t.Fatalf("base Commit: %v", err)
	}
	ridA, _, err := repo.Commit(libfossil.CommitOpts{
		Comment: "branch A", User: "test", ParentID: ridBase,
		Files: []libfossil.FileToCommit{{Name: "conflict.txt", Content: []byte("local version\n")}},
	})
	if err != nil {
		t.Fatalf("branch A Commit: %v", err)
	}
	ridB, _, err := repo.Commit(libfossil.CommitOpts{
		Comment: "branch B", User: "test", ParentID: ridBase,
		Files: []libfossil.FileToCommit{{Name: "conflict.txt", Content: []byte("remote version\n")}},
	})
	if err != nil {
		t.Fatalf("branch B Commit: %v", err)
	}

	// CreateCheckout initializes to tip, which (last write wins in the
	// leaf table) is branch A here since it was committed first among the
	// two leaves and branch B follows — extract explicitly to branch A to
	// pin the starting point regardless of tip resolution.
	checkoutDir := filepath.Join(dir, "wt")
	checkout, err := repo.CreateCheckout(checkoutDir, libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	if err := checkout.Extract(ridA, libfossil.ExtractOpts{Force: true}); err != nil {
		t.Fatalf("Extract ridA: %v", err)
	}

	result, err := checkout.Update(libfossil.UpdateOpts{TargetRID: ridB})
	if err != nil {
		t.Fatalf("conflicted Update must still succeed (err==nil): %v", err)
	}
	if len(result.Conflicted) != 1 || result.Conflicted[0] != "conflict.txt" {
		t.Fatalf("expected Conflicted=[conflict.txt], got %v", result.Conflicted)
	}

	data, err := os.ReadFile(filepath.Join(checkoutDir, "conflict.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<<<<<<<") {
		t.Fatalf("expected conflict markers written to disk, got %q", data)
	}
}
