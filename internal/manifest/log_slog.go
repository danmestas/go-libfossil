//go:build !libfossil_noslog

package manifest

import "log/slog"

// The package's diagnostics go to log/slog's default logger, unless the build
// sets libfossil_noslog (see log_noslog.go).

func logDebug(msg string, args ...any) { slog.Debug(msg, args...) }
func logInfo(msg string, args ...any)  { slog.Info(msg, args...) }
func logWarn(msg string, args ...any)  { slog.Warn(msg, args...) }
