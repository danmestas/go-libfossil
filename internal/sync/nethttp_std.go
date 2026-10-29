//go:build !tinygo && !libfossil_nohttp

package sync

import "net/http"

// errStopRedirect makes the client hand back a redirect response instead of
// following it, so PostXfer applies the sync protocol's own redirect rules.
var errStopRedirect = http.ErrUseLastResponse
