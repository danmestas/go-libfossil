package simio

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MemStorage is an in-memory Storage implementation for tests and browser fallback.
type MemStorage struct {
	files  map[string][]byte
	dirs   map[string]bool
	mtimes map[string]time.Time
	writes int64 // stamps each write: the nth write is dated n seconds after 1970
}

// NewMemStorage creates a new in-memory storage.
func NewMemStorage() *MemStorage {
	return &MemStorage{
		files:  make(map[string][]byte),
		dirs:   make(map[string]bool),
		mtimes: make(map[string]time.Time),
	}
}

// Stat returns file info for the given path.
func (m *MemStorage) Stat(path string) (os.FileInfo, error) {
	if path == "" {
		panic("MemStorage.Stat: path must not be empty")
	}
	// Check files first
	if data, ok := m.files[path]; ok {
		return &memFileInfo{
			name:  filepath.Base(path),
			size:  int64(len(data)),
			isDir: false,
			mtime: m.mtimes[path],
		}, nil
	}

	// Check directories
	if m.dirs[path] {
		return &memFileInfo{
			name:  filepath.Base(path),
			size:  0,
			isDir: true,
		}, nil
	}

	return nil, fmt.Errorf("stat %s: %w", path, os.ErrNotExist)
}

// Remove deletes the file or empty directory at the given path.
func (m *MemStorage) Remove(path string) error {
	if path == "" {
		panic("MemStorage.Remove: path must not be empty")
	}
	if _, ok := m.files[path]; ok {
		delete(m.files, path)
		return nil
	}
	if m.dirs[path] {
		delete(m.dirs, path)
		return nil
	}
	return os.ErrNotExist
}

// MkdirAll creates the directory and all parent directories.
func (m *MemStorage) MkdirAll(path string, perm os.FileMode) error {
	// Mark all parent paths as directories
	parts := strings.Split(path, string(filepath.Separator))
	current := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		if current == "" {
			current = string(filepath.Separator) + part
		} else {
			current = filepath.Join(current, part)
		}
		m.dirs[current] = true
	}
	return nil
}

// ReadFile reads the file at the given path. Returns a copy of the data.
func (m *MemStorage) ReadFile(path string) ([]byte, error) {
	if path == "" {
		panic("MemStorage.ReadFile: path must not be empty")
	}
	data, ok := m.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	// Return a copy to prevent external modification
	result := make([]byte, len(data))
	copy(result, data)
	return result, nil
}

// WriteFile writes data to the file at the given path. Stores a copy.
func (m *MemStorage) WriteFile(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		panic("MemStorage.WriteFile: path must not be empty")
	}
	if data == nil {
		panic("MemStorage.WriteFile: data must not be nil")
	}
	// Store a copy to prevent external modification
	stored := make([]byte, len(data))
	copy(stored, data)
	m.files[path] = stored
	// A write moves the file's mtime, as on a filesystem; a clock of its own
	// keeps that deterministic and never repeats a time.
	m.writes++
	m.mtimes[path] = time.Unix(m.writes, 0)
	return nil
}

// ReadDir returns directory entries for the given path.
func (m *MemStorage) ReadDir(path string) ([]fs.DirEntry, error) {
	if path == "" {
		panic("MemStorage.ReadDir: path must not be empty")
	}

	// Build prefix for scanning
	prefix := path
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}

	// Track unique direct children (files and subdirectories)
	children := make(map[string]bool)
	childIsDir := make(map[string]bool)

	// Scan files
	for filePath := range m.files {
		if strings.HasPrefix(filePath, prefix) {
			remainder := strings.TrimPrefix(filePath, prefix)
			if remainder == "" {
				continue // Skip exact match
			}
			// Extract first path component
			sepIdx := strings.Index(remainder, string(filepath.Separator))
			var childName string
			if sepIdx == -1 {
				// Direct child file
				childName = remainder
				childIsDir[childName] = false
			} else {
				// File in subdirectory
				childName = remainder[:sepIdx]
				childIsDir[childName] = true
			}
			children[childName] = true
		}
	}

	// Scan dirs for empty subdirectories
	for dirPath := range m.dirs {
		if strings.HasPrefix(dirPath, prefix) && dirPath != path {
			remainder := strings.TrimPrefix(dirPath, prefix)
			if remainder == "" {
				continue
			}
			sepIdx := strings.Index(remainder, string(filepath.Separator))
			var childName string
			if sepIdx == -1 {
				childName = remainder
			} else {
				childName = remainder[:sepIdx]
			}
			children[childName] = true
			childIsDir[childName] = true
		}
	}

	// Check if path exists (either as dir or has children)
	if len(children) == 0 && !m.dirs[path] {
		return nil, os.ErrNotExist
	}

	// Build sorted entry list
	entries := make([]fs.DirEntry, 0, len(children))
	for name := range children {
		entries = append(entries, &memDirEntry{
			name:  name,
			isDir: childIsDir[name],
		})
	}

	// Sort by name to match os.ReadDir behavior
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	return entries, nil
}

// Chtimes sets the access and modification times of the named file.
func (m *MemStorage) Chtimes(path string, atime, mtime time.Time) error {
	if path == "" {
		panic("MemStorage.Chtimes: path must not be empty")
	}
	if _, ok := m.files[path]; !ok {
		return fmt.Errorf("chtimes %s: %w", path, os.ErrNotExist)
	}
	m.mtimes[path] = mtime
	return nil
}

// memDirEntry implements fs.DirEntry for in-memory directory entries.
type memDirEntry struct {
	name  string
	isDir bool
}

func (e *memDirEntry) Name() string { return e.name }
func (e *memDirEntry) IsDir() bool  { return e.isDir }
func (e *memDirEntry) Type() fs.FileMode {
	if e.isDir {
		return fs.ModeDir
	}
	return 0
}
func (e *memDirEntry) Info() (fs.FileInfo, error) {
	return &memFileInfo{
		name:  e.name,
		size:  0,
		isDir: e.isDir,
	}, nil
}

// memFileInfo implements os.FileInfo for in-memory files.
type memFileInfo struct {
	name  string
	size  int64
	isDir bool
	mtime time.Time
}

func (f *memFileInfo) Name() string { return f.name }
func (f *memFileInfo) Size() int64  { return f.size }
func (f *memFileInfo) Mode() os.FileMode {
	if f.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}
func (f *memFileInfo) ModTime() time.Time { return f.mtime }
func (f *memFileInfo) IsDir() bool        { return f.isDir }
func (f *memFileInfo) Sys() any           { return nil }
