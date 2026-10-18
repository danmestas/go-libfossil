package simio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOSStorageStat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	// Create a file
	content := []byte("test content")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	storage := OSStorage{}
	info, err := storage.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	if info.Name() != "test.txt" {
		t.Errorf("expected name test.txt, got %s", info.Name())
	}

	if info.Size() != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), info.Size())
	}

	if info.IsDir() {
		t.Error("expected file, got directory")
	}
}

func TestOSStorageStatNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.txt")

	storage := OSStorage{}
	_, err := storage.Stat(path)
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}

	if !os.IsNotExist(err) {
		t.Errorf("expected IsNotExist error, got: %v", err)
	}
}

func TestOSStorageRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	// Create a file
	if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	storage := OSStorage{}
	if err := storage.Remove(path); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Verify file no longer exists
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file still exists after Remove")
	}
}

func TestOSStorageReadWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	storage := OSStorage{}
	content := []byte("test content\nline 2\n")

	// Write file
	if err := storage.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Read file back
	got, err := storage.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if string(got) != string(content) {
		t.Errorf("content mismatch:\nwant: %q\ngot:  %q", content, got)
	}

	// Verify file permissions
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}

	mode := info.Mode()
	if mode.Perm() != 0644 {
		t.Errorf("expected permissions 0644, got %o", mode.Perm())
	}
}

func TestOSStorageMkdirAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c")

	storage := OSStorage{}
	if err := storage.MkdirAll(path, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	// Verify directory exists
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}

	if !info.IsDir() {
		t.Error("expected directory, got file")
	}

	// Verify intermediate directories were created
	for _, subpath := range []string{
		filepath.Join(dir, "a"),
		filepath.Join(dir, "a", "b"),
	} {
		info, err := os.Stat(subpath)
		if err != nil {
			t.Errorf("intermediate directory %s not created: %v", subpath, err)
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be directory", subpath)
		}
	}

	// Verify calling MkdirAll on existing directory is idempotent
	if err := storage.MkdirAll(path, 0755); err != nil {
		t.Errorf("MkdirAll on existing directory failed: %v", err)
	}
}

func TestOSStorageReadFileNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.txt")

	storage := OSStorage{}
	_, err := storage.ReadFile(path)
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}

	if !os.IsNotExist(err) {
		t.Errorf("expected IsNotExist error, got: %v", err)
	}
}

func TestOSStorageWriteFileOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	storage := OSStorage{}

	// Write initial content
	if err := storage.WriteFile(path, []byte("initial"), 0644); err != nil {
		t.Fatalf("initial WriteFile failed: %v", err)
	}

	// Overwrite with new content
	newContent := []byte("overwritten")
	if err := storage.WriteFile(path, newContent, 0600); err != nil {
		t.Fatalf("overwrite WriteFile failed: %v", err)
	}

	// Verify new content
	got, err := storage.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if string(got) != string(newContent) {
		t.Errorf("content mismatch:\nwant: %q\ngot:  %q", newContent, got)
	}

	// Verify file is readable after overwrite (skip permission check — WASI
	// sandboxes may not preserve exact permission bits).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if info.Size() != int64(len(newContent)) {
		t.Errorf("size = %d, want %d", info.Size(), len(newContent))
	}
}

func TestOSStorageRemoveNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.txt")

	storage := OSStorage{}
	err := storage.Remove(path)
	if err == nil {
		t.Fatal("expected error when removing nonexistent file, got nil")
	}

	if !os.IsNotExist(err) {
		t.Errorf("expected IsNotExist error, got: %v", err)
	}
}

func TestMemStorageRoundTrip(t *testing.T) {
	storage := NewMemStorage()
	path := "/test/file.txt"
	content := []byte("test content\nline 2\n")

	// Write file
	if err := storage.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Read file back
	got, err := storage.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if string(got) != string(content) {
		t.Errorf("content mismatch:\nwant: %q\ngot:  %q", content, got)
	}

	// Verify we got a copy, not a reference
	got[0] = 'X'
	original, _ := storage.ReadFile(path)
	if original[0] == 'X' {
		t.Error("ReadFile returned reference to internal data, expected copy")
	}
}

func TestMemStorageStatNotFound(t *testing.T) {
	storage := NewMemStorage()
	path := "/nonexistent.txt"

	_, err := storage.Stat(path)
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}

	// Use errors.Is to check wrapped error
	if !os.IsNotExist(err) && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected IsNotExist error, got: %v", err)
	}
}

func TestMemStorageRemove(t *testing.T) {
	storage := NewMemStorage()
	path := "/test.txt"

	// Create a file
	if err := storage.WriteFile(path, []byte("test"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Verify it exists
	if _, err := storage.Stat(path); err != nil {
		t.Fatalf("Stat failed after WriteFile: %v", err)
	}

	// Remove it
	if err := storage.Remove(path); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Verify it no longer exists
	_, err := storage.Stat(path)
	if !os.IsNotExist(err) && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists after Remove, Stat error: %v", err)
	}
}

func TestMemStorageMkdirAll(t *testing.T) {
	storage := NewMemStorage()
	path := "/a/b/c"

	// Create nested directories
	if err := storage.MkdirAll(path, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	// Verify directory exists
	info, err := storage.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	if !info.IsDir() {
		t.Error("expected directory, got file")
	}

	// Verify intermediate directories were created
	for _, subpath := range []string{"/a", "/a/b"} {
		info, err := storage.Stat(subpath)
		if err != nil {
			t.Errorf("intermediate directory %s not created: %v", subpath, err)
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be directory", subpath)
		}
	}

	// Verify calling MkdirAll on existing directory is idempotent
	if err := storage.MkdirAll(path, 0755); err != nil {
		t.Errorf("MkdirAll on existing directory failed: %v", err)
	}
}

func TestOSStorageReadDir(t *testing.T) {
	dir := t.TempDir()

	// Create a file
	filePath := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(filePath, []byte("test"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Create a subdirectory
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatalf("failed to create subdirectory: %v", err)
	}

	storage := OSStorage{}
	entries, err := storage.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Verify entries (sorted by name)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	expectedNames := []string{"subdir", "test.txt"}
	for i, expected := range expectedNames {
		if names[i] != expected {
			t.Errorf("entry %d: expected name %s, got %s", i, expected, names[i])
		}
	}

	// Verify IsDir
	if entries[0].IsDir() != true {
		t.Errorf("expected subdir to be directory")
	}
	if entries[1].IsDir() != false {
		t.Errorf("expected test.txt to be file")
	}
}

func TestMemStorageReadDir(t *testing.T) {
	storage := NewMemStorage()

	// Create files in root and subdirectory
	if err := storage.WriteFile("/file1.txt", []byte("content1"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := storage.WriteFile("/subdir/file2.txt", []byte("content2"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := storage.WriteFile("/subdir/file3.txt", []byte("content3"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Test ReadDir at root - should show file1.txt and subdir
	entries, err := storage.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir(/) failed: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries at root, got %d", len(entries))
	}

	// Verify entries are sorted by name
	if entries[0].Name() != "file1.txt" {
		t.Errorf("expected first entry to be file1.txt, got %s", entries[0].Name())
	}
	if entries[0].IsDir() {
		t.Errorf("expected file1.txt to be file, not directory")
	}

	if entries[1].Name() != "subdir" {
		t.Errorf("expected second entry to be subdir, got %s", entries[1].Name())
	}
	if !entries[1].IsDir() {
		t.Errorf("expected subdir to be directory, not file")
	}

	// Test ReadDir in subdir
	subdirEntries, err := storage.ReadDir("/subdir")
	if err != nil {
		t.Fatalf("ReadDir(/subdir) failed: %v", err)
	}

	if len(subdirEntries) != 2 {
		t.Fatalf("expected 2 entries in subdir, got %d", len(subdirEntries))
	}

	// Verify files in subdir
	if subdirEntries[0].Name() != "file2.txt" {
		t.Errorf("expected file2.txt, got %s", subdirEntries[0].Name())
	}
	if subdirEntries[1].Name() != "file3.txt" {
		t.Errorf("expected file3.txt, got %s", subdirEntries[1].Name())
	}
}

func TestMemStorageChtimes(t *testing.T) {
	storage := NewMemStorage()
	path := "/test/file.txt"

	storage.WriteFile(path, []byte("content"), 0644)

	mtime := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	if err := storage.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	info, err := storage.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.ModTime().Equal(mtime) {
		t.Fatalf("ModTime = %v, want %v", info.ModTime(), mtime)
	}
}

func TestMemStorageChtimesNotExist(t *testing.T) {
	storage := NewMemStorage()

	err := storage.Chtimes("/nonexistent.txt", time.Now(), time.Now())
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist, got: %v", err)
	}
}

func TestOSStorageChtimes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("content"), 0644)

	storage := OSStorage{}
	mtime := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	if err := storage.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.ModTime().Equal(mtime) {
		t.Fatalf("ModTime = %v, want %v", info.ModTime(), mtime)
	}
}

func TestMemStorageReadDirNotExist(t *testing.T) {
	storage := NewMemStorage()

	_, err := storage.ReadDir("/nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent directory, got nil")
	}

	if !os.IsNotExist(err) && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected IsNotExist error, got: %v", err)
	}
}

// Each write moves a file's mtime forward by a whole second, as a write on a
// real filesystem gives it a new time: checkout scans trust an mtime that
// has not moved.
func TestMemStorageWriteAdvancesMTime(t *testing.T) {
	storage := NewMemStorage()
	path := "/a.txt"
	var last time.Time
	for i := 0; i < 3; i++ {
		if err := storage.WriteFile(path, []byte("same"), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := storage.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && info.ModTime().Unix() <= last.Unix() {
			t.Fatalf("write %d: mtime %v, want after %v", i, info.ModTime(), last)
		}
		last = info.ModTime()
	}
}
