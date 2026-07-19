package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// buildVersion overrides the module version reported by Version, for
// release builds where runtime/debug.ReadBuildInfo cannot report anything
// more meaningful than "(devel)" -- e.g. a plain `go build` invocation on
// the main module outside of a `go install pkg@version` or a git checkout.
// When set, it is used verbatim as the version field: a release build
// script is expected to bake in whatever it considers the canonical
// identifier (tag, commit, or both), so Version does not also append a
// separately-detected commit on top of it. Set at build time:
//
//	go build -ldflags "-X github.com/danmestas/libfossil/cli.buildVersion=v0.6.3" ./cmd/libfossil
var buildVersion = ""

// VersionCmd prints a single-line, stable, machine-parseable build
// identifier and exits 0.
type VersionCmd struct{}

// Run prints Version() and always succeeds: there is nothing about the
// local environment that can make reporting the build identifier fail.
func (c *VersionCmd) Run() error {
	fmt.Println(Version())
	return nil
}

// Version returns a single-line build identifier for this binary: the
// module version (or a build-time override), the Go toolchain version,
// and the platform. The shape is fixed at four whitespace-separated
// fields -- "libfossil <version> <go version> <os>/<arch>" -- regardless
// of which fields carried real data, so a caller can rely on
// strings.Fields(Version()) unconditionally. This is the same string
// both `libfossil version` and `libfossil --version` print.
func Version() string {
	return fmt.Sprintf("libfossil %s %s %s/%s", moduleVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// moduleVersion resolves the version field: the ldflags override when
// present, otherwise whatever runtime/debug.ReadBuildInfo can report.
func moduleVersion() string {
	if buildVersion != "" {
		return buildVersion
	}
	return versionFromBuildInfo()
}

// versionFromBuildInfo derives a version identifier from the running
// binary's embedded build info: the module version when the toolchain
// recorded one, else a short VCS commit, else the literal "(devel)"
// fallback -- never empty.
//
// Main.Version alone is preferred over composing it with vcs.revision:
// the toolchain already folds VCS state into it whenever that state is
// known -- a `go install pkg@vX.Y.Z` reports the tag verbatim, and a
// `go build` inside a version-controlled checkout reports a pseudo-version
// that already embeds the commit and a dirty-tree marker. Appending
// vcs.revision on top would duplicate the same commit hash in that second
// case. The raw commit is only used as a fallback for the one case where
// Main.Version carries no information at all: VCS stamping disabled
// (`-buildvcs=false`) but a checkout was still available at build time.
func versionFromBuildInfo() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}

	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}

	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			return shortCommit(s.Value)
		}
	}

	return "(devel)"
}

// shortCommit truncates a VCS revision to a stable display length,
// matching the convention other Fossil-family tools use for commit
// prefixes in version output.
func shortCommit(rev string) string {
	const shortLen = 12
	if len(rev) > shortLen {
		return rev[:shortLen]
	}
	return rev
}
