package libfossil

import (
	"slices"
	"strings"
	"testing"
)

// noHTTPTag builds the library without its net/http transport and server.
const noHTTPTag = "libfossil_nohttp"

// TestNoHTTPTagLeavesOutNetHTTP: built with the libfossil_nohttp tag, the
// packages an embedder links compile and carry no net/http, whose TLS and
// X.509 dependencies are about 97 KB of a gzipped TinyGo binary that serves
// sync through HandleSyncWithOpts and its host's own transport. Without the
// tag the HTTP transport and server are linked as before.
func TestNoHTTPTagLeavesOutNetHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go tool")
	}
	banned := []string{"net/http", "net/http/httptrace", "crypto/tls"}
	for _, target := range [][2]string{{"", ""}, {"js", "wasm"}} {
		goos, goarch := target[0], target[1]
		goTool(t, goos, goarch, "build", "-tags", noHTTPTag, ".", "./db", "./simio")
		for _, pkg := range []string{".", "./db", "./simio"} {
			deps := strings.Fields(goTool(t, goos, goarch, "list", "-deps", "-tags", noHTTPTag,
				"-f={{.ImportPath}}", pkg))
			for _, b := range banned {
				if slices.Contains(deps, b) {
					t.Errorf("%s (GOOS=%q, -tags %s) links %s", pkg, goos, noHTTPTag, b)
				}
			}
		}
		if !slices.Contains(goListDeps(t, goos, goarch, "."), "net/http") {
			t.Errorf("the library (GOOS=%q) without -tags %s does not link net/http", goos, noHTTPTag)
		}
	}
}
