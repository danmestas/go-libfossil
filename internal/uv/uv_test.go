package uv

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// openTestDB creates a temporary repo DB with schema for testing.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.CreateRepoSchema(d); err != nil {
		d.Close()
		t.Fatalf("CreateRepoSchema: %v", err)
	}
	if err := EnsureSchema(d); err != nil {
		d.Close()
		t.Fatalf("EnsureSchema: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestEnsureSchema(t *testing.T) {
	d := openTestDB(t)
	// Calling twice should be idempotent.
	if err := EnsureSchema(d); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
}

func TestWriteAndRead(t *testing.T) {
	d := openTestDB(t)

	if err := Write(d, "test.txt", []byte("hello world"), 1700000000); err != nil {
		t.Fatalf("Write: %v", err)
	}

	content, mtime, hash, err := Read(d, "test.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(content) != "hello world" {
		t.Errorf("content = %q, want %q", content, "hello world")
	}
	if mtime != 1700000000 {
		t.Errorf("mtime = %d, want %d", mtime, 1700000000)
	}
	if hash == "" {
		t.Error("hash should not be empty")
	}
}

func TestWriteOverwrite(t *testing.T) {
	d := openTestDB(t)
	Write(d, "f.txt", []byte("v1"), 100)
	Write(d, "f.txt", []byte("v2"), 200)

	content, mtime, _, err := Read(d, "f.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(content) != "v2" || mtime != 200 {
		t.Errorf("got content=%q mtime=%d, want v2/200", content, mtime)
	}
}

func TestDeleteAndRead(t *testing.T) {
	d := openTestDB(t)
	Write(d, "test.txt", []byte("hello"), 100)
	if err := Delete(d, "test.txt", 200); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	content, mtime, hash, err := Read(d, "test.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != nil {
		t.Errorf("content should be nil for tombstone, got %q", content)
	}
	if mtime != 200 {
		t.Errorf("mtime = %d, want 200", mtime)
	}
	if hash != "" {
		t.Errorf("hash should be empty for tombstone, got %q", hash)
	}
}

func TestReadNonExistent(t *testing.T) {
	d := openTestDB(t)
	content, mtime, hash, err := Read(d, "nope.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if content != nil || mtime != 0 || hash != "" {
		t.Errorf("expected zero values for non-existent file")
	}
}

func TestList(t *testing.T) {
	d := openTestDB(t)
	Write(d, "a.txt", []byte("aaa"), 100)
	Write(d, "b.txt", []byte("bbb"), 200)
	Delete(d, "c.txt", 300)

	entries, err := List(d)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
}

func TestContentHashEmpty(t *testing.T) {
	d := openTestDB(t)
	h, err := ContentHash(d)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	// SHA1 of empty string
	if h != "da39a3ee5e6b4b0d3255bfef95601890afd80709" {
		t.Errorf("empty hash = %q, want da39a3ee...", h)
	}
}

func TestContentHashDeterministic(t *testing.T) {
	d := openTestDB(t)
	Write(d, "b.txt", []byte("bbb"), 200)
	Write(d, "a.txt", []byte("aaa"), 100)

	h1, _ := ContentHash(d)
	h2, _ := ContentHash(d)
	if h1 != h2 {
		t.Errorf("ContentHash not deterministic: %q != %q", h1, h2)
	}
}

func TestContentHashExcludesTombstones(t *testing.T) {
	d := openTestDB(t)
	Write(d, "a.txt", []byte("aaa"), 100)

	h1, _ := ContentHash(d)

	Delete(d, "b.txt", 200) // tombstone should not affect hash

	h2, _ := ContentHash(d)
	if h1 != h2 {
		t.Errorf("tombstone changed hash: %q != %q", h1, h2)
	}
}

func TestContentHashChangesOnWrite(t *testing.T) {
	d := openTestDB(t)
	Write(d, "a.txt", []byte("v1"), 100)
	h1, _ := ContentHash(d)

	Write(d, "a.txt", []byte("v2"), 200)
	h2, _ := ContentHash(d)

	if h1 == h2 {
		t.Error("hash should change after write")
	}
}

func TestInvalidateHash(t *testing.T) {
	d := openTestDB(t)
	Write(d, "a.txt", []byte("aaa"), 100)
	h1, _ := ContentHash(d) // caches

	// Manually update without going through Write (simulating external change).
	d.Exec("UPDATE unversioned SET mtime=999 WHERE name='a.txt'")
	InvalidateHash(d)

	h2, _ := ContentHash(d)
	if h1 == h2 {
		t.Error("hash should differ after invalidate + mtime change")
	}
}
