//go:build unix

package undo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/undo"
	"github.com/danmestas/go-libfossil/internal/vfile"
	"github.com/danmestas/go-libfossil/simio"
)

// RestoreFiles puts a symlink ReadFile saw back as a symlink, deleting what
// replaced it rather than writing through it, as fossil's undo_rollback
// does; with allow-symlinks off the link is read and written through, as
// fossil's RepoFILE then follows it.
func TestRestoreFilesRestoresSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	for name, body := range map[string]string{"plain.txt": "plain\n", "other.txt": "other\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("plain.txt", link); err != nil {
		t.Fatal(err)
	}
	disk := vfile.NewDisk(simio.RealEnv(), true)
	f, err := undo.ReadFile(disk, dir, "link")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other.txt", link); err != nil {
		t.Fatal(err)
	}
	if err := undo.RestoreFiles(disk, dir, []undo.File{f}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != "plain.txt" {
		t.Fatalf("link: Readlink = %q, %v; want a symlink to plain.txt", got, err)
	}
	for name, body := range map[string]string{"plain.txt": "plain\n", "other.txt": "other\n"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != body {
			t.Fatalf("%s = %q, %v; want %q", name, data, err, body)
		}
	}

	off := vfile.NewDisk(simio.RealEnv(), false)
	if f, err = undo.ReadFile(off, dir, "link"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := undo.RestoreFiles(off, dir, []undo.File{f}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != "plain.txt" {
		t.Fatalf("link with allow-symlinks off: Readlink = %q, %v; want it kept", got, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "plain.txt")); err != nil || string(data) != "plain\n" {
		t.Fatalf("plain.txt = %q, %v; want its saved content back", data, err)
	}
}
