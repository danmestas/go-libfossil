//go:build !js

package libfossil_test

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/danmestas/go-libfossil"
)

// stdoutOf runs fn and returns what it wrote to os.Stdout.
func stdoutOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestCheckoutUndoReportsFiles: Undo and Redo of an update return the files
// they changed, for the caller to show, and print nothing themselves.
func TestCheckoutUndoReportsFiles(t *testing.T) {
	dir := t.TempDir()
	repo, err := libfossil.Create(filepath.Join(dir, "undo.fossil"), libfossil.CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	rid1, _, err := repo.Commit(libfossil.CommitOpts{
		Comment: "first", User: "test",
		Files: []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("v1")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rid2, _, err := repo.Commit(libfossil.CommitOpts{
		Comment: "second", User: "test", ParentID: rid1,
		Files: []libfossil.FileToCommit{{Name: "a.txt", Content: []byte("v2")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := repo.CreateCheckout(filepath.Join(dir, "wt"), libfossil.CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	if err := checkout.Extract(rid1, libfossil.ExtractOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := checkout.Update(libfossil.UpdateOpts{TargetRID: rid2}); err != nil {
		t.Fatal(err)
	}

	var undone, redone libfossil.UndoResult
	out := stdoutOf(t, func() {
		if undone, err = checkout.Undo(); err != nil {
			t.Fatal(err)
		}
		if redone, err = checkout.Redo(); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Errorf("Undo and Redo printed %q", out)
	}
	want := libfossil.UndoResult{FilesRestored: []string{"a.txt"}}
	if !reflect.DeepEqual(undone, want) {
		t.Errorf("Undo = %+v, want %+v", undone, want)
	}
	if !reflect.DeepEqual(redone, want) {
		t.Errorf("Redo = %+v, want %+v", redone, want)
	}
}
