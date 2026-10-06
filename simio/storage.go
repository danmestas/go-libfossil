package simio

import (
	"io/fs"
	"os"
	"time"
)

// Storage abstracts filesystem operations for WASM portability.
type Storage interface {
	Stat(path string) (os.FileInfo, error)
	Remove(path string) error
	MkdirAll(path string, perm os.FileMode) error
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	ReadDir(path string) ([]fs.DirEntry, error)
	Chtimes(path string, atime, mtime time.Time) error
}

// OSStorage delegates to the os package. Used in production and WASI.
type OSStorage struct{}

func (OSStorage) Stat(path string) (os.FileInfo, error)                       { return os.Stat(path) }
func (OSStorage) Remove(path string) error                                     { return os.Remove(path) }
func (OSStorage) MkdirAll(path string, perm os.FileMode) error                { return os.MkdirAll(path, perm) }
func (OSStorage) ReadFile(path string) ([]byte, error)                         { return os.ReadFile(path) }
func (OSStorage) WriteFile(path string, data []byte, perm os.FileMode) error  { return os.WriteFile(path, data, perm) }
func (OSStorage) ReadDir(path string) ([]fs.DirEntry, error)                  { return os.ReadDir(path) }
func (OSStorage) Chtimes(path string, atime, mtime time.Time) error           { return os.Chtimes(path, atime, mtime) }

// Chmod changes a file's mode. It is not part of Storage, since not every
// storage has file modes; callers that need it check for it.
func (OSStorage) Chmod(path string, mode os.FileMode) error { return os.Chmod(path, mode) }
