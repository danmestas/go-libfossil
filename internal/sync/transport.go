package sync

import (
	"context"

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
