package sync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/testutil"
)

// HTTPTransport follows redirects as fossil's http_exchange does (src/http.c,
// draft-fossil-sync-protocol-00 §4). See also transport_redirect_test.go in
// the root package, which covers the public transport.

// emptyReply answers with a decodable, empty xfer message.
func emptyReply(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	body, err := (&xfer.Message{}).Encode()
	if err != nil {
		t.Errorf("encode reply: %v", err)
		return
	}
	w.Header().Set("Content-Type", xfer.ContentTypeCompressed)
	w.Write(body)
}

func pingMessage() *xfer.Message {
	return &xfer.Message{Cards: []xfer.Card{&xfer.PragmaCard{Name: "client-version"}}}
}

func TestHTTPTransportRePostsAfter302(t *testing.T) {
	want, err := pingMessage().Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var method string
	var got []byte
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		got, _ = io.ReadAll(r.Body)
		emptyReply(t, w)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer origin.Close()

	tr := &HTTPTransport{URL: origin.URL}
	if _, err := tr.Exchange(context.Background(), pingMessage()); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if method != http.MethodPost || !bytes.Equal(got, want) {
		t.Fatalf("redirected as %s with %d bytes, want POST with the same %d bytes",
			method, len(got), len(want))
	}
}

func TestHTTPTransportStripsTrailingXferOnce(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/repo/xfer/xfer", http.StatusTemporaryRedirect)
			return
		}
		path = r.URL.Path
		emptyReply(t, w)
	}))
	defer srv.Close()
	tr := &HTTPTransport{URL: srv.URL + "/"}
	if _, err := tr.Exchange(context.Background(), pingMessage()); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if path != "/repo/xfer" {
		t.Fatalf("posted to %s, want /repo/xfer", path)
	}
}

func TestHTTPTransportStopsAtRedirectLimit(t *testing.T) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, srv.URL+"/again", http.StatusPermanentRedirect)
	}))
	defer srv.Close()
	tr := &HTTPTransport{URL: srv.URL}
	_, err := tr.Exchange(context.Background(), pingMessage())
	if !errors.Is(err, ErrRedirectLimit) {
		t.Fatalf("err = %v, want ErrRedirectLimit", err)
	}
	if n := hits.Load(); n != 20 {
		t.Fatalf("sent %d requests, want 20", n)
	}
}

func TestPostXferRefusesDowngrade(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.RedirectHandler(plain.URL, http.StatusPermanentRedirect))
	defer secure.Close()

	_, err := PostXfer(context.Background(), secure.Client(), secure.URL,
		xfer.ContentTypeCompressed, "", []byte("login card"))
	if !errors.Is(err, ErrRedirectDowngrade) {
		t.Fatalf("err = %v, want ErrRedirectDowngrade", err)
	}
	if retryableExchangeFault(err) {
		t.Fatal("a refused downgrade must not be retried")
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("the request went to the plain-HTTP target %d times", n)
	}
}

// TestCloneFromRealFossilThroughRedirect clones from fossil serve behind a
// front that answers 302 with the server's /xfer URL: the body must be
// re-POSTed and /xfer stripped for fossil to answer the clone.
func TestCloneFromRealFossilThroughRedirect(t *testing.T) {
	bin := testutil.RequireFossilBin(t)
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.fossil")
	if out, err := exec.Command(bin, "new", remote).CombinedOutput(); err != nil {
		t.Fatalf("fossil new: %v\n%s", err, out)
	}
	url := startFossilServer(t, remote)
	front := httptest.NewServer(http.RedirectHandler(url+"/xfer", http.StatusFound))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, res, err := Clone(ctx, filepath.Join(dir, "local.fossil"),
		&HTTPTransport{URL: front.URL}, CloneOpts{})
	if err != nil {
		t.Fatalf("Clone through redirect: %v", err)
	}
	defer r.Close()
	if res.BlobsRecvd == 0 || res.ProjectCode != getProjectCode(t, remote) {
		t.Fatalf("clone got %d blobs, project %q", res.BlobsRecvd, res.ProjectCode)
	}
}
