package sync

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	body, err := req.Encode()
	if err != nil {
		return nil, fmt.Errorf("sync.HTTPTransport encode: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("sync.HTTPTransport request: %w", err)
	}
	httpReq.Header.Set("Content-Type", xfer.ContentTypeCompressed)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sync.HTTPTransport do: %w", err)
	}
	defer resp.Body.Close()
	// An error page is not an xfer reply: report the status rather than
	// handing HTML to the decoder, which would report the peer's outage as a
	// framing defect. Whether the status is worth another attempt is
	// StatusError's to say.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	respBody, err := io.ReadAll(resp.Body)
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
	return xfer.Decode(respBody, resp.Header.Get("Content-Type"))
}
