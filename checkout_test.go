package libfossil

import (
	"os"
	"path/filepath"
	"testing"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestCheckoutCreateAndExtract(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "test.fossil")
	coDir := filepath.Join(dir, "work")

	r, err := Create(repoPath, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Create a commit with a file.
	rid, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "hello.txt", Content: []byte("hello world\n")},
		},
		Comment: "initial commit",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create checkout and extract.
	co, err := r.CreateCheckout(coDir, CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if co.Dir() != coDir {
		t.Errorf("Dir() = %q, want %q", co.Dir(), coDir)
	}

	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Verify the file exists on disk.
	data, err := os.ReadFile(filepath.Join(coDir, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world\n" {
		t.Errorf("file content = %q, want %q", string(data), "hello world\n")
	}

	// Verify Version returns the correct RID.
	vRID, vUUID, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if vRID != rid {
		t.Errorf("Version RID = %d, want %d", vRID, rid)
	}
	if vUUID == "" {
		t.Error("Version UUID is empty")
	}
}

func TestCheckoutStatus(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "test.fossil")
	coDir := filepath.Join(dir, "work")

	r, err := Create(repoPath, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "a.txt", Content: []byte("aaa\n")},
		},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	co, err := r.CreateCheckout(coDir, CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Initially no changes.
	has, err := co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("HasChanges = true before modification, want false")
	}

	// Modify the file on disk.
	if err := os.WriteFile(filepath.Join(coDir, "a.txt"), []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Status should detect the modification.
	changes, err := co.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("Status returned %d changes, want 1", len(changes))
	}
	if changes[0].Name != "a.txt" {
		t.Errorf("changed file = %q, want %q", changes[0].Name, "a.txt")
	}
	if changes[0].Change != "modified" {
		t.Errorf("change type = %q, want %q", changes[0].Change, "modified")
	}

	// HasChanges should now be true (ScanChanges was called by Status).
	has, err = co.HasChanges()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("HasChanges = false after modification, want true")
	}
}

func TestCheckoutCheckin(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "test.fossil")
	coDir := filepath.Join(dir, "work")

	r, err := Create(repoPath, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Initial commit.
	rid, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "a.txt", Content: []byte("aaa\n")},
		},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	co, err := r.CreateCheckout(coDir, CheckoutCreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Add a new file to the working directory and track it.
	newFile := filepath.Join(coDir, "b.txt")
	if err := os.WriteFile(newFile, []byte("bbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	added, err := co.Add([]string{"b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("Add returned %d, want 1", added)
	}

	// Commit from the checkout.
	newRID, newUUID, err := co.Checkin(CheckoutCommitOpts{
		Message: "add b.txt",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if newRID <= rid {
		t.Errorf("new RID %d should be > parent RID %d", newRID, rid)
	}
	if newUUID == "" {
		t.Error("Checkin returned empty UUID")
	}

	// Version should reflect the new commit.
	vRID, _, err := co.Version()
	if err != nil {
		t.Fatal(err)
	}
	if vRID != newRID {
		t.Errorf("Version RID = %d, want %d", vRID, newRID)
	}

	// The repo should now have both files in the new checkin.
	files, err := r.ListFiles(newRID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("ListFiles returned %d files, want 2", len(files))
	}
}
