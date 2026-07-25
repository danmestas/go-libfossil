package libfossil

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// framedFuncTransport is a FramedTransport test double: it returns fixed reply
// bytes and a fixed framing signal, so the adapter's Content-Type-driven decode
// path can be exercised without a network.
type framedFuncTransport struct {
	resp        []byte
	contentType string
}

func (f *framedFuncTransport) RoundTrip(ctx context.Context, _ []byte) ([]byte, error) {
	return f.resp, nil
}

func (f *framedFuncTransport) RoundTripFramed(ctx context.Context, _ []byte) ([]byte, string, error) {
	return f.resp, f.contentType, nil
}

// pragmaMessage builds a one-card message resembling the opening card of a real
// Fossil clone reply, whose plain text begins with "pragma".
func pragmaMessage(t *testing.T) *xfer.Message {
	t.Helper()
	return &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "server-version", Values: []string{"2.28"}},
	}}
}

func firstPragmaName(t *testing.T, m *xfer.Message) string {
	t.Helper()
	if len(m.Cards) != 1 {
		t.Fatalf("decoded %d cards, want 1", len(m.Cards))
	}
	p, ok := m.Cards[0].(*xfer.PragmaCard)
	if !ok {
		t.Fatalf("decoded card is %T, want *xfer.PragmaCard", m.Cards[0])
	}
	return p.Name
}

// TestExchangeFramedUncompressed is the unit-level companion to the real-fossil
// clone regression: when the transport reports the uncompressed framing signal,
// the adapter must decode a plain-card reply as plain cards, not force-read it
// as the compressed container.
func TestExchangeFramedUncompressed(t *testing.T) {
	body, err := pragmaMessage(t).EncodeUncompressed()
	if err != nil {
		t.Fatalf("EncodeUncompressed: %v", err)
	}
	adapter := &transportAdapter{pub: &framedFuncTransport{
		resp:        body,
		contentType: xfer.ContentTypeUncompressed,
	}}
	got, err := adapter.Exchange(context.Background(), &xfer.Message{})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if name := firstPragmaName(t, got); name != "server-version" {
		t.Fatalf("pragma name = %q, want server-version", name)
	}
}

// TestExchangeFramedCompressed confirms the compressed framing still decodes
// through the same Content-Type-driven path when the signal says compressed.
func TestExchangeFramedCompressed(t *testing.T) {
	body, err := pragmaMessage(t).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	adapter := &transportAdapter{pub: &framedFuncTransport{
		resp:        body,
		contentType: xfer.ContentTypeCompressed,
	}}
	got, err := adapter.Exchange(context.Background(), &xfer.Message{})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if name := firstPragmaName(t, got); name != "server-version" {
		t.Fatalf("pragma name = %q, want server-version", name)
	}
}

// TestExchangeUnframedFallbackCompressed confirms that a Transport with no
// framing signal (MockTransport, which does not implement FramedTransport)
// still decodes via the hardcoded compressed fallback — the peer-to-peer case
// where both ends always emit the §4.1 compressed container.
func TestExchangeUnframedFallbackCompressed(t *testing.T) {
	body, err := pragmaMessage(t).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, isFramed := interface{}(&MockTransport{}).(FramedTransport); isFramed {
		t.Fatal("MockTransport must not implement FramedTransport; it represents the no-signal case")
	}
	adapter := &transportAdapter{pub: &MockTransport{
		Handler: func([]byte) []byte { return body },
	}}
	got, err := adapter.Exchange(context.Background(), &xfer.Message{})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if name := firstPragmaName(t, got); name != "server-version" {
		t.Fatalf("pragma name = %q, want server-version", name)
	}
}
