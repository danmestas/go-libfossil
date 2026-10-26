package libfossil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestHTTPTransportRoundTrip(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/x-fossil" {
			t.Errorf("Content-Type = %q, want application/x-fossil", ct)
		}
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	}))
	defer echo.Close()

	tr := NewHTTPTransport(echo.URL)
	resp, err := tr.RoundTrip(context.Background(), []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "hello" {
		t.Errorf("got %q, want %q", resp, "hello")
	}
}

func TestMockTransportRoundTrip(t *testing.T) {
	tr := &MockTransport{
		Handler: func(req []byte) []byte { return []byte("mock") },
	}
	resp, err := tr.RoundTrip(context.Background(), []byte("req"))
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "mock" {
		t.Errorf("got %q, want %q", resp, "mock")
	}
}

func TestTransportFunc(t *testing.T) {
	called := false
	fn := TransportFunc(func(ctx context.Context, payload []byte) ([]byte, error) {
		called = true
		return []byte("response"), nil
	})
	resp, err := fn.RoundTrip(context.Background(), []byte("request"))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !called {
		t.Error("handler function was not called")
	}
	if string(resp) != "response" {
		t.Errorf("got %q, want %q", resp, "response")
	}
}

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

// The HTTP transport follows redirects as fossil's http_exchange does
// (src/http.c, draft-fossil-sync-protocol-00 §4): never from HTTPS to HTTP,
// re-POSTing the same body for 301, 302, 307 and 308, stripping one trailing
// /xfer, and failing on the twentieth redirect.

// redirectTo answers every request with status code and a Location of to.
func redirectTo(code int, to func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", to())
		w.WriteHeader(code)
	}
}

func TestHTTPTransportRefusesHTTPSToHTTPRedirect(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		w.Header().Set("Content-Type", "application/x-fossil")
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(redirectTo(http.StatusTemporaryRedirect,
		func() string { return plain.URL }))
	defer secure.Close()

	tr := NewHTTPTransport(secure.URL, WithHTTPClient(secure.Client()))
	_, err := tr.RoundTrip(context.Background(), []byte("login card"))
	if err == nil {
		t.Fatal("redirect from HTTPS to HTTP was followed")
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("the request body went to the plain-HTTP target %d times", n)
	}
}

func TestHTTPTransportRePostsAfterRedirect(t *testing.T) {
	payload := []byte("cards to send")
	for _, code := range []int{301, 302, 307, 308} {
		var method, ctype string
		var got []byte
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, ctype = r.Method, r.Header.Get("Content-Type")
			got, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/x-fossil")
			w.Write([]byte("reply"))
		}))
		origin := httptest.NewServer(redirectTo(code, func() string { return target.URL }))

		tr := NewHTTPTransport(origin.URL)
		resp, err := tr.RoundTrip(context.Background(), payload)
		origin.Close()
		target.Close()
		if err != nil {
			t.Fatalf("%d: RoundTrip: %v", code, err)
		}
		if method != http.MethodPost || ctype != "application/x-fossil" {
			t.Errorf("%d: redirected as %s %q, want POST application/x-fossil", code, method, ctype)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("%d: redirected body = %q, want %q", code, got, payload)
		}
		if string(resp) != "reply" {
			t.Errorf("%d: reply = %q", code, resp)
		}
	}
}

func TestHTTPTransportStripsOneTrailingXfer(t *testing.T) {
	for _, tc := range []struct{ location, want string }{
		{"/repo/xfer", "/repo"},
		{"/repo/xfer/xfer", "/repo/xfer"},
	} {
		var path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				w.Header().Set("Location", tc.location)
				w.WriteHeader(http.StatusFound)
				return
			}
			path = r.URL.Path
			w.Header().Set("Content-Type", "application/x-fossil")
		}))
		_, err := NewHTTPTransport(srv.URL+"/").RoundTrip(context.Background(), []byte("x"))
		srv.Close()
		if err != nil {
			t.Fatalf("%s: RoundTrip: %v", tc.location, err)
		}
		if path != tc.want {
			t.Errorf("redirect to %s posted to %s, want %s", tc.location, path, tc.want)
		}
	}
}

// chainServer redirects until it has answered hops requests, then replies.
func chainServer(hops int32) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= hops {
			w.Header().Set("Location", srv.URL+"/r")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/x-fossil")
	}))
	return srv, &hits
}

func TestHTTPTransportRedirectLimit(t *testing.T) {
	ok, _ := chainServer(19)
	defer ok.Close()
	if _, err := NewHTTPTransport(ok.URL).RoundTrip(context.Background(), []byte("x")); err != nil {
		t.Fatalf("19 redirects should be followed: %v", err)
	}

	endless, hits := chainServer(1 << 30)
	defer endless.Close()
	_, err := NewHTTPTransport(endless.URL).RoundTrip(context.Background(), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "redirect limit") {
		t.Fatalf("err = %v, want the redirect limit", err)
	}
	if n := hits.Load(); n != 20 {
		t.Fatalf("sent %d requests, want 20: the twentieth redirect is fatal", n)
	}
}
