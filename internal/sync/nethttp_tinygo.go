//go:build tinygo

package sync

import "errors"

// errStopRedirect: TinyGo's net/http has no ErrUseLastResponse, so a redirect
// ends the request with this error rather than being followed. A TinyGo
// build normally syncs through a transport its host supplies, not PostXfer.
var errStopRedirect = errors.New("sync: redirect not followed under TinyGo")

// dnsFault: TinyGo's net package has no DNSError, so no failure is classified
// as name resolution; the other network checks still apply.
func dnsFault(error) (transient, ok bool) { return false, false }
