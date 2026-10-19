//go:build !unix

package simio

import (
	"io/fs"
	"os"
)

// CheckWritable reports whether this process may write path, where there
// is no access(2): a regular file without the owner-write bit, which is
// how Windows reports a read-only file, is not writable; anything else is
// taken to be. It is not part of Storage, since not every storage has
// permissions; callers that need it check for it.
func (OSStorage) CheckWritable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() && info.Mode().Perm()&0o200 == 0 {
		return &os.PathError{Op: "access", Path: path, Err: fs.ErrPermission}
	}
	return nil
}
