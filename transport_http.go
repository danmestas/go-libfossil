//go:build !libfossil_nohttp

package libfossil

// This file holds the net/http sync client. A build with the libfossil_nohttp tag
// leaves it out, and with it net/http, for a host that supplies its own
// transport (docs: Install, TinyGo).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	internalsync "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

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
