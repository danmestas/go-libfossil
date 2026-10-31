//go:build !libfossil_noslog

package manifest

// slogBuild reports whether diagnostics go to log/slog, where tests can
// capture them; with libfossil_noslog they go to stderr (log_noslog.go).
const slogBuild = true
