package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"unicode"
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

// init rejects a malformed buildVersion at program startup, before any
// command runs -- not just when someone happens to run `version`. A
// whitespace-containing override is a build-time operator mistake (a
// broken -ldflags invocation), not a runtime condition: sanitizing it
// would silently mangle the operator's intent, and falling back to
// runtime/debug info would report a version that is confidently wrong.
// Both hide the mistake in the one field whose entire job is to be
// trustworthy for the benchmark harness that stamps it into every
// emitted record. Failing the whole binary immediately surfaces the bad
// build the first time it runs, rather than shipping a corrupted
// identifier that only breaks downstream when someone parses it.
func init() {
	assertValidBuildVersion(buildVersion)
}

// assertValidBuildVersion panics if v would break Version()'s single-line,
// four-field contract. Empty is valid -- it means no -ldflags override was
// supplied and Version falls back to runtime/debug.ReadBuildInfo.
func assertValidBuildVersion(v string) {
	if v == "" {
		return
	}
	if strings.ContainsFunc(v, unicode.IsSpace) {
		panic(fmt.Sprintf("cli: buildVersion (set via -ldflags -X) must not contain whitespace, got %q", v))
	}
}

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
// recorded one, else the literal "(devel)" fallback -- never empty.
//
// Main.Version alone is used verbatim, with no separate vcs.revision
// composition: empirically (verified against go1.26 with `go build .`
// inside this repo's own checkout), Main.Version is only ever the
// uninformative literal "(devel)" in exactly the cases where VCS
// stamping produced nothing at all -- -buildvcs=false, or no VCS
// checkout present -- and in both, bi.Settings carries no vcs.revision
// either. Whenever a VCS revision *is* available, Go has already folded
// it into Main.Version as a pseudo-version (e.g.
// "v0.6.4-20260719231612-e972f9ffa769+dirty"), so a fallback loop over
// vcs.revision would either never fire or double-report the same commit.
func versionFromBuildInfo() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" {
		return "(devel)"
	}
	return bi.Main.Version
}
