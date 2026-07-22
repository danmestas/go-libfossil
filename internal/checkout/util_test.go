package checkout

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/simio"
)

func TestFileContent(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()

	// Use MemStorage to capture extracted files
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Extract files
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Read via FileContent
	content, err := co.FileContent("hello.txt")
	if err != nil {
		t.Fatal("FileContent failed:", err)
	}
	if string(content) != "hello world\n" {
		t.Fatalf("FileContent = %q, want %q", content, "hello world\n")
	}

	// Read nested file
	content2, err := co.FileContent("src/main.go")
	if err != nil {
		t.Fatal("FileContent src/main.go failed:", err)
	}
	if string(content2) != "package main\n" {
		t.Fatalf("FileContent src/main.go = %q", content2)
	}

	// Non-existent file should error
	_, err = co.FileContent("nonexistent.txt")
	if err == nil {
		t.Fatal("FileContent should fail for non-existent file")
	}
}

func TestWriteManifest(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Use MemStorage
	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	_, uuid, _ := co.Version()

	// Write both manifest files
	if err := co.WriteManifest(ManifestMain | ManifestUUID); err != nil {
		t.Fatal("WriteManifest failed:", err)
	}

	// Verify manifest file exists and contains content
	manifestData, err := mem.ReadFile("/checkout/manifest")
	if err != nil {
		t.Fatal("manifest file not found:", err)
	}
	if len(manifestData) == 0 {
		t.Fatal("manifest file is empty")
	}
	// Check that it looks like a manifest (should start with "C ")
	if !strings.HasPrefix(string(manifestData), "C ") {
		t.Fatalf("manifest content doesn't look like a manifest: %q", string(manifestData[:min(50, len(manifestData))]))
	}

	// Verify manifest.uuid file
	uuidData, err := mem.ReadFile("/checkout/manifest.uuid")
	if err != nil {
		t.Fatal("manifest.uuid file not found:", err)
	}
	if string(uuidData) != uuid+"\n" {
		t.Fatalf("manifest.uuid = %q, want %q", uuidData, uuid+"\n")
	}
}

func TestWriteManifestFlags(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	mem := simio.NewMemStorage()
	co.env = &simio.Env{Storage: mem, Clock: simio.RealClock{}, Rand: simio.CryptoRand{}}
	co.dir = "/checkout"

	// Write only UUID
	if err := co.WriteManifest(ManifestUUID); err != nil {
		t.Fatal("WriteManifest ManifestUUID failed:", err)
	}

	// manifest.uuid should exist
	if _, err := mem.ReadFile("/checkout/manifest.uuid"); err != nil {
		t.Fatal("manifest.uuid not found:", err)
	}

	// manifest should NOT exist
	if _, err := mem.ReadFile("/checkout/manifest"); err == nil {
		t.Fatal("manifest should not exist when only ManifestUUID is set")
	}

	// Clear storage
	mem = simio.NewMemStorage()
	co.env.Storage = mem

	// Write only main manifest
	if err := co.WriteManifest(ManifestMain); err != nil {
		t.Fatal("WriteManifest ManifestMain failed:", err)
	}

	// manifest should exist
	if _, err := mem.ReadFile("/checkout/manifest"); err != nil {
		t.Fatal("manifest not found:", err)
	}

	// manifest.uuid should NOT exist
	if _, err := mem.ReadFile("/checkout/manifest.uuid"); err == nil {
		t.Fatal("manifest.uuid should not exist when only ManifestMain is set")
	}
}

func TestCheckFilename(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"simple relative", "hello.txt", "hello.txt", false},
		{"nested relative", "src/main.go", "src/main.go", false},
		{"with dots", "./src/../hello.txt", "hello.txt", false},
		{"escape parent", "../etc/passwd", "", true},
		{"escape complex", "../../etc/passwd", "", true},
		{"just dotdot", "..", "", true},
		{"dotdot prefix", "../sibling", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := co.CheckFilename(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckFilename(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("CheckFilename(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCheckFilenameAbsolute(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	// Absolute path within checkout
	absInside := filepath.Join(dir, "hello.txt")
	got, err := co.CheckFilename(absInside)
	if err != nil {
		t.Errorf("CheckFilename(%q) unexpected error: %v", absInside, err)
	}
	if got != "hello.txt" {
		t.Errorf("CheckFilename(%q) = %q, want %q", absInside, got, "hello.txt")
	}

	// Absolute path outside checkout
	absOutside := "/etc/passwd"
	_, err = co.CheckFilename(absOutside)
	if err == nil {
		t.Errorf("CheckFilename(%q) should fail for path outside checkout", absOutside)
	}
}

func TestIsRootedIn(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	tests := []struct {
		name    string
		absPath string
		want    bool
	}{
		{"inside exact", dir, true},
		{"inside nested", filepath.Join(dir, "subdir", "file.txt"), true},
		{"inside single file", filepath.Join(dir, "file.txt"), true},
		{"outside parent", filepath.Dir(dir), false},
		{"outside sibling", filepath.Join(filepath.Dir(dir), "sibling"), false},
		{"outside root", "/etc/passwd", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := co.IsRootedIn(tt.absPath)
			if got != tt.want {
				t.Errorf("IsRootedIn(%q) = %v, want %v", tt.absPath, got, tt.want)
			}
		})
	}
}

func TestSafePathRejectsTraversal(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	bad := []string{"../etc/passwd", "../../root/.ssh/id_rsa", "/etc/shadow", "foo/../../bar"}
	for _, p := range bad {
		_, err := co.safePath(p)
		if err == nil {
			t.Fatalf("safePath(%q) should have failed", p)
		}
	}

	good := []string{"hello.txt", "src/main.go", "a/b/c.txt"}
	for _, p := range good {
		path, err := co.safePath(p)
		if err != nil {
			t.Fatalf("safePath(%q) failed: %v", p, err)
		}
		if path == "" {
			t.Fatalf("safePath(%q) returned empty", p)
		}
		if !strings.HasPrefix(path, dir) {
			t.Fatalf("safePath(%q) = %q, not within checkout dir %q", p, path, dir)
		}
	}
}

func TestFileContentRejectsTraversal(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	_, err = co.FileContent("../etc/passwd")
	if err == nil {
		t.Fatal("FileContent(\"../etc/passwd\") should have failed")
	}
	if !strings.Contains(err.Error(), "escapes checkout") {
		t.Fatalf("FileContent error should mention escapes checkout, got: %v", err)
	}
}

func TestRenameRejectsTraversal(t *testing.T) {
	r, cleanup := newTestRepoWithCheckin(t)
	defer cleanup()

	dir := t.TempDir()
	co, err := Create(r, dir, CreateOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	rid, _, _ := co.Version()
	if err := co.Extract(rid, ExtractOpts{}); err != nil {
		t.Fatal(err)
	}

	// Rename To with traversal should fail
	err = co.Rename(RenameOpts{
		From:     "hello.txt",
		To:       "../outside",
		DoFsMove: true,
	})
	if err == nil {
		t.Fatal("Rename to ../outside should have failed")
	}
	if !strings.Contains(err.Error(), "escapes checkout") {
		t.Fatalf("Rename error should mention escapes checkout, got: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
