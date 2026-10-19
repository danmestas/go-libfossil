//go:build unix

package simio

import (
	"os"
	"syscall"
)

// wOK is access(2)'s W_OK.
const wOK = 0x2

// CheckWritable reports, with access(2), whether this process may write
// path: a file's content, or a directory's entries. It is not part of
// Storage, since not every storage has permissions; callers that need it
// check for it.
func (OSStorage) CheckWritable(path string) error {
	if err := syscall.Access(path, wOK); err != nil {
		return &os.PathError{Op: "access", Path: path, Err: err}
	}
	return nil
}
