//go:build !libfossil_nohttp

package libfossil

// This file holds the net/http sync server. A build with the libfossil_nohttp tag
// leaves it out, and with it net/http, for a host that supplies its own
// transport (docs: Install, TinyGo).

import (
	"context"
	"net/http"

	internalsync "github.com/danmestas/go-libfossil/internal/sync"
)

// XferHandler returns an http.HandlerFunc that decodes Fossil xfer requests,
// dispatches to HandleSync, and encodes the response. Use this to compose
// a custom mux alongside operational endpoints (e.g., /healthz). It serves
// with default HandleOpts, so no HandleOpts.MaxStoredBytes budget applies.
func (r *Repo) XferHandler() http.HandlerFunc {
	h := internalsync.HandleSync
	return internalsync.XferHandler(r.inner, h)
}

// ServeHTTP starts an HTTP server that accepts Fossil xfer requests.
// Blocks until ctx is cancelled.
//
// Requests are authorized against the user table: one without a login card
// gets nobody's capabilities, which in a new repository allow clone and pull
// but not push. Grant more with SetCaps. Like XferHandler it serves with
// default HandleOpts, so no HandleOpts.MaxStoredBytes budget applies.
func (r *Repo) ServeHTTP(ctx context.Context, addr string) error {
	return internalsync.ServeHTTP(ctx, addr, r.inner, internalsync.HandleSync)
}
