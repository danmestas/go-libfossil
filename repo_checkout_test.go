package libfossil

import (
	"path/filepath"
	"testing"

	_ "github.com/danmestas/libfossil/internal/testdriver"
)

func TestCommitAndAncestry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid1, uuid, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "hello.txt", Content: []byte("hello world\n")},
		},
		Comment: "initial commit",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if uuid == "" {
		t.Fatal("Commit returned empty UUID")
	}
	if len(uuid) != 40 && len(uuid) != 64 {
		t.Errorf("UUID length = %d, want 40 or 64", len(uuid))
	}
	if rid1 <= 0 {
		t.Fatalf("Commit returned rid=%d, want > 0", rid1)
	}

	// Second commit to test the ancestry walk.
	rid2, uuid2, err := r.Commit(CommitOpts{
		ParentID: rid1,
		Files: []FileToCommit{
			{Name: "hello.txt", Content: []byte("hello world v2\n")},
		},
		Comment: "second commit",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := r.Ancestry(LogOpts{Start: rid2, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("Ancestry returned %d entries, want >= 2", len(entries))
	}
	if entries[0].UUID != uuid2 {
		t.Errorf("first entry UUID = %q, want %q", entries[0].UUID, uuid2)
	}
	if entries[0].Comment != "second commit" {
		t.Errorf("first entry Comment = %q, want %q", entries[0].Comment, "second commit")
	}
	if entries[1].UUID != uuid {
		t.Errorf("second entry UUID = %q, want %q", entries[1].UUID, uuid)
	}
}

// TestCommitPreservesParentFiles is the regression for #30: a child
// commit that supplies only a subset of the parent's tracked files must
// still produce a full-tree manifest. Before the fix, the child's
// manifest contained only the supplied subset and a checkout at the
// child's rev silently lost every file the caller did not re-supply.
func TestCommitPreservesParentFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	parentRID, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "a.txt", Content: []byte("alpha\n")},
			{Name: "b.txt", Content: []byte("bravo v1\n")},
			{Name: "c.txt", Content: []byte("charlie\n")},
		},
		Comment: "parent: a, b, c",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("parent commit: %v", err)
	}

	childRID, _, err := r.Commit(CommitOpts{
		ParentID: parentRID,
		Files: []FileToCommit{
			{Name: "b.txt", Content: []byte("bravo v2\n")},
		},
		Comment: "child: only b modified",
		User:    "test",
	})
	if err != nil {
		t.Fatalf("child commit: %v", err)
	}

	files, err := r.ListFiles(childRID)
	if err != nil {
		t.Fatalf("ListFiles(child): %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Name] = f.UUID
	}
	if len(got) != 3 {
		t.Fatalf("child manifest has %d files, want 3 (a.txt, b.txt, c.txt); got %v", len(got), got)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, ok := got[name]; !ok {
			t.Errorf("child manifest missing %q (file tracked at parent was dropped)", name)
		}
	}

	// a.txt and c.txt should keep the parent's blob UUIDs (untouched).
	parentFiles, err := r.ListFiles(parentRID)
	if err != nil {
		t.Fatalf("ListFiles(parent): %v", err)
	}
	parentUUIDs := map[string]string{}
	for _, f := range parentFiles {
		parentUUIDs[f.Name] = f.UUID
	}
	if got["a.txt"] != parentUUIDs["a.txt"] {
		t.Errorf("a.txt UUID changed across commits: parent=%s child=%s", parentUUIDs["a.txt"], got["a.txt"])
	}
	if got["c.txt"] != parentUUIDs["c.txt"] {
		t.Errorf("c.txt UUID changed across commits: parent=%s child=%s", parentUUIDs["c.txt"], got["c.txt"])
	}
	// b.txt should have a NEW UUID — caller supplied new content.
	if got["b.txt"] == parentUUIDs["b.txt"] {
		t.Errorf("b.txt UUID unchanged despite new content: %s", got["b.txt"])
	}
}

func TestListFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid, _, err := r.Commit(CommitOpts{
		Files: []FileToCommit{
			{Name: "a.txt", Content: []byte("aaa\n")},
			{Name: "b.txt", Content: []byte("bbb\n")},
		},
		Comment: "two files",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	files, err := r.ListFiles(rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("ListFiles returned %d files, want 2", len(files))
	}
	if files[0].Name != "a.txt" || files[1].Name != "b.txt" {
		t.Errorf("files = %v, want a.txt and b.txt", files)
	}
}
