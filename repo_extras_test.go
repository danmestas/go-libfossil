package libfossil

import (
	"path/filepath"
	"testing"
	"time"

	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

func TestUVWriteReadList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	now := time.Now()
	if err := r.UVWrite("wiki/hello.md", []byte("# Hello\n"), now); err != nil {
		t.Fatal(err)
	}

	content, mtime, hash, err := r.UVRead("wiki/hello.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# Hello\n" {
		t.Errorf("UVRead content = %q, want %q", content, "# Hello\n")
	}
	if mtime == 0 {
		t.Error("UVRead mtime = 0, want nonzero")
	}
	if hash == "" {
		t.Error("UVRead hash is empty")
	}

	entries, err := r.UVList()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("UVList returned %d entries, want 1", len(entries))
	}
	if entries[0].Name != "wiki/hello.md" {
		t.Errorf("UVList[0].Name = %q, want %q", entries[0].Name, "wiki/hello.md")
	}
}

func TestTagCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	rid, _, err := r.Commit(CommitOpts{
		Files:   []FileToCommit{{Name: "f.txt", Content: []byte("x\n")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	tagRid, err := r.Tag(TagOpts{
		Name:     "sym-v1.0",
		TargetID: rid,
		User:     "test",
		Time:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tagRid <= 0 {
		t.Errorf("Tag returned rid=%d, want > 0", tagRid)
	}
}

func TestConfigGetSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.fossil")

	r, err := Create(path, CreateOpts{User: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// project-code is set by Create
	pc, err := r.Config("project-code")
	if err != nil {
		t.Fatal(err)
	}
	if pc == "" {
		t.Error("project-code should not be empty")
	}

	if err := r.SetConfig("custom-key", "custom-value"); err != nil {
		t.Fatal(err)
	}
	val, err := r.Config("custom-key")
	if err != nil {
		t.Fatal(err)
	}
	if val != "custom-value" {
		t.Errorf("Config(custom-key) = %q, want %q", val, "custom-value")
	}
}
