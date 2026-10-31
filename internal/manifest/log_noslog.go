//go:build libfossil_noslog

package manifest

import (
	"fmt"
	"os"
	"strings"
)

// With libfossil_noslog the package's diagnostics are written to stderr as
// plain lines, "LEVEL msg key=value ...", so log/slog stays out of the build.
// It is for size-bound builds such as TinyGo WebAssembly, where slog costs
// about 33 KB gzipped. Debug messages are dropped, as slog's default level
// drops them. Tests that assert on slog output need the default build.

func logDebug(string, ...any) {}

func logInfo(msg string, args ...any) { logLine("INFO", msg, args) }

func logWarn(msg string, args ...any) { logLine("WARN", msg, args) }

func logLine(level, msg string, args []any) {
	var b strings.Builder
	b.WriteString(level)
	b.WriteByte(' ')
	b.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	if len(args)%2 == 1 {
		fmt.Fprintf(&b, " !BADKEY=%v", args[len(args)-1])
	}
	b.WriteByte('\n')
	_, _ = os.Stderr.WriteString(b.String())
}
