//go:build !tinygo

package checkout

import "os"

// sameFile is os.SameFile: whether two names are one file (a hard link).
func sameFile(a, b os.FileInfo) bool { return os.SameFile(a, b) }
