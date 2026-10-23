//go:build !tinygo

package sync

import (
	"errors"
	"net"
	"net/http"
)

// errStopRedirect makes the client hand back a redirect response instead of
// following it, so PostXfer applies the sync protocol's own redirect rules.
var errStopRedirect = http.ErrUseLastResponse

// dnsFault reports whether err is a name-resolution failure (ok), and if so
// whether retrying could help: a temporary failure can pass, but a name that
// does not exist will not start existing.
func dnsFault(err error) (transient, ok bool) {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return false, false
	}
	return dnsErr.IsTemporary || dnsErr.IsTimeout, true
}
