//go:build tinygo && !libfossil_nohttp

package sync

import "errors"

// errStopRedirect: TinyGo's net/http has no ErrUseLastResponse, so a redirect
// ends the request with this error rather than being followed. A TinyGo
// build normally syncs through a transport its host supplies, not PostXfer.
var errStopRedirect = errors.New("sync: redirect not followed under TinyGo")
