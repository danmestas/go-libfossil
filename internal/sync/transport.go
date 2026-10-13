package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Transport sends an xfer request and returns the response.
// Implementations handle encoding (the request Message is already decoded),
// network I/O, and decoding. The xfer.Message uses zlib-compressed payloads
// internally — see [xfer.Encode] and [xfer.Decode] for wire format details.
//
// Built-in implementations: [HTTPTransport] (Fossil HTTP /xfer protocol),
// [MockTransport] (canned responses for testing). The leaf agent adds
// NATSTransport for NATS-based peer-to-peer sync.
type Transport interface {
	Exchange(ctx context.Context, request *xfer.Message) (*xfer.Message, error)
}

// MockTransport replays canned responses for testing.
type MockTransport struct {
	Handler func(req *xfer.Message) *xfer.Message
}

func (t *MockTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	if req == nil {
		panic("sync.MockTransport.Exchange: req must not be nil")
	}
	if t.Handler == nil {
		return &xfer.Message{}, nil
	}
	return t.Handler(req), nil
}

// HTTPTransport speaks Fossil's HTTP /xfer protocol.
// Fossil routes to /xfer based on Content-Type: application/x-fossil,
// NOT the URL path. URL should be the repo root (e.g. "http://localhost:8080").
type HTTPTransport struct {
	URL string // repo root, e.g. "http://localhost:8080"
}

func (t *HTTPTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	if req == nil {
		panic("sync.HTTPTransport.Exchange: req must not be nil")
	}
	sent, cookie := req.SplitLoginCookie()
	body, err := sent.Encode()
	if err != nil {
		return nil, fmt.Errorf("sync.HTTPTransport encode: %w", err)
	}
	resp, err := PostXfer(ctx, http.DefaultClient, t.URL, xfer.ContentTypeCompressed, cookie,
		body)
	if err != nil {
		return nil, fmt.Errorf("sync.HTTPTransport do: %w", err)
	}
	defer resp.Body.Close()
	// Only 200 is a sync reply (§4-7). Any other status, a 2xx included, is not
	// the protocol answering: report it rather than hand its body to the
	// decoder, which would read the peer's outage as a framing defect. Whether
	// the status is worth another attempt is StatusError's to say.
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	respBody, err := xfer.ReadBody(resp.Body)
	if errors.Is(err, xfer.ErrBodyTooLarge) {
		// The reply is the peer's, and resending gets the same one.
		return nil, err
	}
	if err != nil {
		// The reply was cut short on the wire. Nothing has been interpreted
		// yet, so the same request may safely be sent again.
		return nil, &transportFault{
			err:       fmt.Errorf("sync.HTTPTransport read: %w", err),
			retryable: true,
		}
	}
	// §4: the reply's framing is given by its Content-Type, not guessed. A
	// clone-v3 reply arrives uncompressed; a pull reply arrives as the
	// compressed container.
	//
	// A failure from here on is a protocol fault: the reply arrived whole and
	// did not parse. It is left untagged so the retry classifier fails it
	// fast — resending would only make a malformed reply intermittent.
	// §4-8: a reply with no Content-Type is the compressed container, an
	// x-fossil-debug or x-fossil-uncompressed type is plain cards, x-fossil is
	// the compressed container, and any foreign type (an HTML error page from a
	// proxy) is not a sync reply and is refused here.
	return xfer.DecodeReply(respBody, resp.Header.Get("Content-Type"))
}
