//go:build !js

package libfossil_test

// Interop tests for undoing an update (#446): the undo state go-libfossil's
// Update saves is fossil's, so `fossil undo` reverses it, and Checkout.Undo
// reverses `fossil update`.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
)

// undoState is what an undo must restore: the files on disk, what fossil's
// changes reports, and the checked-out version.
func undoState(t *testing.T, bin, dir string) string {
	t.Helper()
	var files []string
	for name, body := range treeFiles(t, dir) {
		files = append(files, name+"="+body)
	}
	sort.Strings(files)
	return strings.Join(files, "\n") + "\n" + fossilRun(t, bin, dir, "changes") +
		fossilRun(t, bin, dir, "sql",
			"SELECT value FROM vvar WHERE name IN ('checkout','checkout-hash') ORDER BY name")
}

// TestFossilUndoReversesLibfossilUpdate: go-libfossil updates a checkout
// with local edits, a pending add and removal; fossil's undo puts it back.
func TestFossilUndoReversesLibfossilUpdate(t *testing.T) {
	bin, repoPath, a, b := updateEditsRepo(t)
	ck, goDir, ridOf := goCheckoutAt(t, bin, repoPath, a)
	localEdits(t, goDir)
	if _, err := ck.Add([]string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := ck.Remove([]string{"same.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ck.Status(); err != nil {
		t.Fatal(err)
	}
	before := undoState(t, bin, goDir)

	if _, err := ck.Update(libfossil.UpdateOpts{TargetRID: ridOf(b)}); err != nil {
		t.Fatal(err)
	}
	// The binary conflict's backup is not undo state, in fossil either.
	if err := os.Remove(filepath.Join(goDir, "bin.dat-original")); err != nil {
		t.Fatal(err)
	}
	fossilRun(t, bin, goDir, "undo")
	if got := undoState(t, bin, goDir); got != before {
		t.Fatalf("after fossil undo:\n%s\nwant, as before the update:\n%s", got, before)
	}
}

// TestLibfossilUndoReversesFossilUpdate: fossil updates the same checkout
// state; go-libfossil's Undo puts it back.
func TestLibfossilUndoReversesFossilUpdate(t *testing.T) {
	bin, repoPath, a, b := updateEditsRepo(t)
	dir := t.TempDir()
	fossilRun(t, bin, dir, "open", repoPath, a)
	localEdits(t, dir)
	fossilRun(t, bin, dir, "add", "new.txt")
	fossilRun(t, bin, dir, "rm", "same.txt")
	fossilRun(t, bin, dir, "changes")
	before := undoState(t, bin, dir)

	fossilRun(t, bin, dir, "update", b)
	// The binary conflict's backup is not undo state.
	if err := os.Remove(filepath.Join(dir, "bin.dat-original")); err != nil {
		t.Fatal(err)
	}
	ck := openLibfossilCheckout(t, repoPath, dir)
	if _, err := ck.Undo(); err != nil {
		t.Fatal(err)
	}
	if err := ck.Close(); err != nil {
		t.Fatal(err)
	}
	if got := undoState(t, bin, dir); got != before {
		t.Fatalf("after Undo:\n%s\nwant, as before fossil's update:\n%s", got, before)
	}
}
