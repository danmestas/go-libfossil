package libfossil

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
