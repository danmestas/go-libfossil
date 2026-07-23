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
	httpReq.Header.Set("Content-Type", "application/x-fossil")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sync.HTTPTransport do: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sync.HTTPTransport read: %w", err)
	}
	return xfer.Decode(respBody)
}
