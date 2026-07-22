//go:build js

package repo

import "github.com/danmestas/go-libfossil/simio"

// checkExists skips the stat check on js/wasm — OPFS handles file
// existence via the VFS, not the OS filesystem.
func checkExists(_ *simio.Env, _ string) error { return nil }
