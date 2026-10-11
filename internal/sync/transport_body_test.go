package sync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// endlessBodyServer replies 200 with a body that does not end until the
// client stops reading. It stops on its own after 1 GiB, so a client that
// reads without limit fails the test rather than hanging it.
func endlessBodyServer(t testing.TB) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", xfer.ContentTypeUncompressed)
		chunk := make([]byte, 64<<10)
		for sent := 0; sent < 1<<30; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil || r.Context().Err() != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHTTPTransportCapsResponseBody checks that a reply longer than
// xfer.MaxBodyBytes is refused once the cap is passed. Method: point the
// transport at a server streaming an unbounded body. The transport used to
// read the whole reply into memory, so a hostile server could exhaust the
// client's memory with one response.
func TestHTTPTransportCapsResponseBody(t *testing.T) {
	srv := endlessBodyServer(t)
	tr := &HTTPTransport{URL: srv.URL}
	_, err := tr.Exchange(context.Background(), &xfer.Message{})
	if !errors.Is(err, xfer.ErrBodyTooLarge) {
		t.Fatalf("Exchange error = %v, want xfer.ErrBodyTooLarge", err)
	}
	if retryableExchangeFault(err) {
		t.Fatal("an oversize reply was tagged retryable; resending gets the same reply")
	}
}
