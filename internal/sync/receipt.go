package sync

import (
	"context"
	"fmt"
	"net"
	gosync "sync"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// A sync, a clone and a request the server answers are each one receipt, as
// each is one fossil process holding one g.rcvid (src/content.c,
// content_rcvid_init): the first content one stores records an rcvfrom row,
// and everything it stores names that row. This file holds what fills those
// rows the way fossil fills them.

// peerAddresser is a Transport that knows the address of the peer it last
// exchanged with, as fossil's client knows g.zIpAddr: the numeric address of
// the socket it connected (src/http_socket.c). A transport that does not
// know leaves the client's receipt without one, as fossil's file transport
// does.
type peerAddresser interface {
	PeerAddr() string
}

// PeerAddr records the IP address of the peer an HTTP request connected to.
// The zero value is ready to use and safe for concurrent use.
type PeerAddr struct {
	mu gosync.Mutex
	ip string
}

// Get returns the address last recorded, "" before any.
func (p *PeerAddr) Get() string {
	if p == nil {
		panic("sync.PeerAddr.Get: receiver must not be nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ip
}

// hostOnly drops the port from a host:port address, as REMOTE_ADDR and
// fossil's getnameinfo carry the address alone.
func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// withClientReceipt returns r recording what a sync or clone receives under
// one receipt, credited to the local user named login (fossil's user_select)
// and stamped by clock. A Repo already carrying a receipt keeps it, so the
// pull a clone runs for its phantoms is part of the clone's receipt.
func withClientReceipt(r *repo.Repo, login string, clock simio.Clock) (*repo.Repo, error) {
	if r.Receipt() != nil {
		return r, nil
	}
	uid, err := r.LocalUID(login)
	if err != nil {
		return nil, fmt.Errorf("receipt: %w", err)
	}
	return r.WithReceipt(&db.Receipt{UID: uid, Clock: clock}), nil
}

// notePeer gives the client's receipt the address of the peer t last
// exchanged with. It matters only until the receipt's row is made, which is
// when fossil reads g.zIpAddr.
func notePeer(r *repo.Repo, t Transport) {
	rc := r.Receipt()
	if rc == nil {
		panic("sync.notePeer: the session's repo has no receipt")
	}
	if p, ok := t.(peerAddresser); ok {
		rc.IPAddr = p.PeerAddr()
	}
}

// remoteAddrKey is the context key for the address a request came from.
type remoteAddrKey struct{}

// WithRemoteAddr returns ctx carrying addr, the address a sync request came
// from, for the server's receipt (fossil's REMOTE_ADDR). A host:port address
// is recorded as the host alone.
func WithRemoteAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, remoteAddrKey{}, hostOnly(addr))
}

// serverReceipt is the receipt of one request the server answers: credited to
// nobody until a login card names a user (src/login.c sets g.userUid to
// nobody's uid before page_xfer reads any card), from the request's address.
func serverReceipt(ctx context.Context, r *repo.Repo, opts HandleOpts) (*db.Receipt, error) {
	var uid int64
	err := r.DB().QueryRow("SELECT coalesce((SELECT uid FROM user WHERE login='nobody'), 0)").
		Scan(&uid)
	if err != nil {
		return nil, fmt.Errorf("receipt: nobody's uid: %w", err)
	}
	addr := hostOnly(opts.RemoteAddr)
	if addr == "" {
		addr, _ = ctx.Value(remoteAddrKey{}).(string)
	}
	return &db.Receipt{UID: uid, IPAddr: addr, Clock: opts.Clock}, nil
}
