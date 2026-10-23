//go:build tinygo

package checkout

import "os"

// sameFile: TinyGo's os has no SameFile, so two names are never taken for
// one file. A linked marker pair is then reported as ambiguous rather than
// recovered, which refuses instead of guessing.
func sameFile(os.FileInfo, os.FileInfo) bool { return false }
