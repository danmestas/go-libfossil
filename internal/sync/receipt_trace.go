//go:build !libfossil_nohttp

package sync

// This file holds the net/http peer address trace. A build with the libfossil_nohttp tag
// leaves it out, and with it net/http, for a host that supplies its own
// transport (docs: Install, TinyGo).

import (
	"context"
	"net/http/httptrace"
)

// Trace returns ctx with an HTTP trace that records the address of the
// connection a request made with it goes out on.
func (p *PeerAddr) Trace(ctx context.Context) context.Context {
	if p == nil {
		panic("sync.PeerAddr.Trace: receiver must not be nil")
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn == nil {
				return
			}
			ip := hostOnly(info.Conn.RemoteAddr().String())
			p.mu.Lock()
			p.ip = ip
			p.mu.Unlock()
		},
	})
}
