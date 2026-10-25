package libfossil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	internalsync "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Transport delivers sync payloads between peers.
// Implementations handle the network layer (HTTP, NATS, etc.).
// Payloads are opaque zlib-compressed xfer card streams.
type Transport interface {
	RoundTrip(ctx context.Context, payload []byte) ([]byte, error)
}

// FramedTransport is an optional interface a Transport may also implement when
// it can report the framing signal — such as an HTTP Content-Type header — that
// accompanied a reply. Sync uses that signal to select the reply's §4 framing
// instead of assuming the §4.1 compressed container, which matters when the
// peer is a different Fossil-protocol implementation that legitimately replies
// with the uncompressed plain-card framing (e.g. a real Fossil server's clone
// reply).
//
// It is optional in the same way http.Flusher is optional on an
// http.ResponseWriter: a Transport whose framing is fixed by convention (both
// ends always emit the compressed container) need not implement it, and sync
// falls back to assuming the compressed framing for such a Transport. Callers
// type-assert a Transport to FramedTransport and use RoundTripFramed when it is
// available. Reporting the signal as a return value keeps it per-call, with no
// shared mutable state to make a reused Transport unsafe across concurrent
// calls.
type FramedTransport interface {
	Transport
	// RoundTripFramed behaves like RoundTrip but also returns the framing
	// signal (e.g. the reply's Content-Type) the wire carried for this reply,
	// or "" when the transport observed none.
	RoundTripFramed(ctx context.Context, payload []byte) (resp []byte, contentType string, err error)
}

// NewHTTPTransport creates a Transport that speaks Fossil's HTTP /xfer protocol.
func NewHTTPTransport(url string, opts ...HTTPOption) Transport {
	t := &httpTransport{url: url, client: http.DefaultClient}
	for _, o := range opts {
		o(t)
	}
	return t
}

// HTTPOption configures an HTTP transport.
type HTTPOption func(*httpTransport)

// WithHTTPClient sets a custom http.Client for the transport.
func WithHTTPClient(c *http.Client) HTTPOption {
	return func(t *httpTransport) { t.client = c }
}

// OriginNamer is an optional interface a Transport may also implement to
// name the peer it addresses. Sync and Clone keep the horizon cursor of
// draft-fossil-horizon-00 §6.2.6 per origin under that name: "A client that
// converged records the cursor of the last reply together with the count of
// its own artifacts at the end of the exchange, per origin", and "A client
// holding a cursor for the origin it addresses sends it on every request,
// and MAY then announce nothing with igot on a pull-only request."
//
// The name must identify the same peer across sessions and must carry no
// credential, since it is written into the repository's config table; the
// HTTP transport uses its URL with any password removed. A Transport that
// does not implement it, or returns "", keeps no cursor, and each of its
// pulls announces the client's whole holding with igot, which §6.2.6 allows.
type OriginNamer interface {
	Transport
	SyncOrigin() string
}

// syncOrigin is the origin t names, or "" when it names none.
func syncOrigin(t Transport) string {
	if o, ok := t.(OriginNamer); ok {
		return o.SyncOrigin()
	}
	return ""
}

type httpTransport struct {
	url    string
	client *http.Client
	peer   internalsync.PeerAddr
}

// SyncOrigin names the peer this transport addresses, for state a sync keeps
// per origin (the horizon cursor of draft-fossil-horizon-00 §6.2.6): its URL
// with any password removed, as last-sync-url keeps it, so no credential is
// written into the repository.
func (t *httpTransport) SyncOrigin() string {
	u, err := url.Parse(t.url)
	if err != nil {
		return ""
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// PeerAddr returns the IP address of the server the last round trip reached.
func (t *httpTransport) PeerAddr() string {
	return t.peer.Get()
}

func (t *httpTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	body, _, err := t.roundTrip(ctx, payload, "")
	return body, err
}

// RoundTripFramed implements FramedTransport: it returns the reply body along
// with the reply's Content-Type, which selects the §4 framing for decoding. A
// peer that replies uncompressed (e.g. a real Fossil clone reply) is decoded
// correctly instead of being force-read as the compressed container.
func (t *httpTransport) RoundTripFramed(ctx context.Context, payload []byte) ([]byte, string, error) {
	return t.roundTrip(ctx, payload, "")
}

// loginCookieTransport is a Transport that can carry a login card in the
// x-f-l-c cookie rather than the body (§6.5), as the HTTP transport can.
type loginCookieTransport interface {
	roundTripLoginCookie(ctx context.Context, payload []byte,
		cookie string) ([]byte, string, error)
}

func (t *httpTransport) roundTripLoginCookie(ctx context.Context, payload []byte,
	cookie string) ([]byte, string, error) {
	return t.roundTrip(ctx, payload, cookie)
}

func (t *httpTransport) roundTrip(ctx context.Context, payload []byte,
	cookie string) ([]byte, string, error) {
	resp, err := internalsync.PostXfer(t.peer.Trace(ctx), t.client, t.url,
		"application/x-fossil", cookie, payload)
	if err != nil {
		return nil, "", fmt.Errorf("libfossil: http transport: %w", err)
	}
	defer resp.Body.Close()
	// Only 200 is a sync reply (§4-7). Any other status, a 2xx included, is not
	// the protocol answering. Reporting the status directly keeps a peer's
	// outage from surfacing as a framing defect, and lets the sync round loop
	// tell a 5xx worth retrying from a 4xx that never will be.
	if resp.StatusCode != http.StatusOK {
		return nil, "", &internalsync.StatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	body, err := xfer.ReadBody(resp.Body)
	if errors.Is(err, xfer.ErrBodyTooLarge) {
		// The reply is the peer's, and resending gets the same one.
		return nil, "", err
	}
	if err != nil {
		// The reply was cut short on the wire; nothing has been decoded yet,
		// so the round loop may safely send this request again. Tagging it
		// here is what distinguishes a severed connection from a body that
		// arrived whole and would not decode — both surface as
		// io.ErrUnexpectedEOF, and only the former is worth retrying.
		return nil, "", internalsync.RetryableFault(
			fmt.Errorf("libfossil: http transport read: %w", err))
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// MockTransport is a test double that delegates to a handler function.
type MockTransport struct {
	Handler func(req []byte) []byte
}

// RoundTrip calls the Handler function if set, otherwise returns empty bytes.
func (t *MockTransport) RoundTrip(_ context.Context, payload []byte) ([]byte, error) {
	if t.Handler == nil {
		return []byte{}, nil
	}
	return t.Handler(payload), nil
}

// TransportFunc adapts a plain function to the Transport interface.
// This is the Transport equivalent of http.HandlerFunc.
type TransportFunc func(ctx context.Context, payload []byte) ([]byte, error)

// RoundTrip calls the function.
func (f TransportFunc) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	return f(ctx, payload)
}
