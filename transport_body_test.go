package libfossil

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestNewHTTPTransportCapsResponseBody checks that the public HTTP transport
// refuses a reply longer than xfer.MaxBodyBytes. Method: stream an unbounded
// body from a test server. The transport used to read the whole reply into
// memory, so a hostile server could exhaust the client's memory. The server
// stops itself after 1 GiB, so an unbounded read fails rather than hangs.
func TestNewHTTPTransportCapsResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", xfer.ContentTypeUncompressed)
		chunk := make([]byte, 64<<10)
		for sent := 0; sent < 1<<30; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil || r.Context().Err() != nil {
				return
			}
		}
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL)
	_, err := tr.RoundTrip(context.Background(), []byte("x"))
	if !errors.Is(err, xfer.ErrBodyTooLarge) {
		t.Fatalf("RoundTrip error = %v, want xfer.ErrBodyTooLarge", err)
	}
}
