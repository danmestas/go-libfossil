package libfossil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Create refuses a path whose parent the filesystem confirms is missing or
// is not a directory, and proceeds when the filesystem cannot say, as under
// TinyGo wasm, where a repository lives in a SQLite VFS and os.Stat is
// unsupported (#509).
func TestCreateParentDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(dir, "no-such-dir", "r.fossil"): "parent directory does not exist",
		filepath.Join(file, "r.fossil"):               "parent is not a directory",
	} {
		_, err := Create(path, CreateOpts{User: "test"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Create(%s) = %v, want %q", path, err, want)
		}
	}

	statUnsupported := func(string) (fs.FileInfo, error) { return nil, errors.ErrUnsupported }
	saved := statParent
	statParent = statUnsupported
	t.Cleanup(func() { statParent = saved })
	r, err := Create(filepath.Join(dir, "r.fossil"), CreateOpts{User: "test"})
	if err != nil {
		t.Fatalf("Create with an unsupported stat: %v, want it to proceed", err)
	}
	r.Close()
}
