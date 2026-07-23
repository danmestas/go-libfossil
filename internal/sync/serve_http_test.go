package sync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestServeHTTPRoundTrip(t *testing.T) {
	r := setupSyncTestRepo(t)
	data := []byte("http test blob")
	uuid := hash.SHA1(data)
	storeReceivedFile(r, uuid, "", data, nil)

	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	transport := &HTTPTransport{URL: fmt.Sprintf("http://%s", addr)}
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.GimmeCard{UUID: uuid},
	}}

	resp, err := transport.Exchange(ctx, req)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}

	files := findCards[*xfer.FileCard](resp)
	found := false
	for _, f := range files {
		if f.UUID == uuid && string(f.Content) == string(data) {
			found = true
		}
	}
	if !found {
		t.Fatal("expected file card in HTTP response")
	}
}

func TestServeHTTPPushPull(t *testing.T) {
	r := setupSyncTestRepo(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	transport := &HTTPTransport{URL: fmt.Sprintf("http://%s", addr)}

	// Push a blob
	data := []byte("pushed via http")
	uuid := hash.SHA1(data)

	pushReq := &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.FileCard{UUID: uuid, Content: data},
	}}
	pushResp, err := transport.Exchange(ctx, pushReq)
	if err != nil {
		t.Fatalf("push exchange: %v", err)
	}
	errs := findCards[*xfer.ErrorCard](pushResp)
	if len(errs) > 0 {
		t.Fatalf("push error: %s", errs[0].Message)
	}

	// Pull it back
	pullReq := &xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
		&xfer.GimmeCard{UUID: uuid},
	}}
	pullResp, err := transport.Exchange(ctx, pullReq)
	if err != nil {
		t.Fatalf("pull exchange: %v", err)
	}

	files := findCards[*xfer.FileCard](pullResp)
	found := false
	for _, f := range files {
		if f.UUID == uuid && string(f.Content) == string(data) {
			found = true
		}
	}
	if !found {
		t.Fatal("pushed blob not available via pull")
	}
}

func TestServeHTTPGetProbe(t *testing.T) {
	r := setupSyncTestRepo(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	// GET should return HTML, not an error.
	resp, err := http.Get(fmt.Sprintf("http://%s/", addr))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET status: %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "text/html" {
		t.Fatalf("GET content-type: %s", ct)
	}
}

func TestServeHTTPEmptyPost(t *testing.T) {
	r := setupSyncTestRepo(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	// Empty POST should return an empty xfer response, not 400.
	resp, err := http.Post(fmt.Sprintf("http://%s/", addr), "application/x-fossil", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("empty POST status: %d", resp.StatusCode)
	}
}

func TestServeHTTPBadPayload(t *testing.T) {
	r := setupSyncTestRepo(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	// Send binary garbage that isn't valid zlib or card data.
	// The Decode fallback to uncompressed will try to parse cards,
	// which may succeed on some text. Use NUL bytes to force failure.
	garbage := make([]byte, 50)
	for i := range garbage {
		garbage[i] = 0xFF
	}
	resp, err := http.Post(
		fmt.Sprintf("http://%s/", addr),
		"application/x-fossil",
		strings.NewReader(string(garbage)),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	// Decode fallback to uncompressed may produce empty cards or error.
	// Either 200 (empty response) or 400 (decode error) is acceptable
	// since the data has no valid cards.
	if resp.StatusCode != 200 && resp.StatusCode != 400 {
		t.Fatalf("bad payload status: %d, want 200 or 400", resp.StatusCode)
	}
}

// TestServeHTTPUnknownContentType pins that a body whose Content-Type is absent
// or unrecognised is rejected outright rather than decoded as plain card text.
// A peer is untrusted input: without a §4 media type nothing says how the body
// is framed, and guessing "uncompressed" reopens issue #104, where still-
// compressed bytes were fed to the card parser.
func TestServeHTTPUnknownContentType(t *testing.T) {
	r := setupSyncTestRepo(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	// A compressed container sent without the media type that identifies it.
	body, err := (&xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	for _, ct := range []string{"", "text/plain", "application/octet-stream"} {
		req, err := http.NewRequest("POST", fmt.Sprintf("http://%s/", addr),
			strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		} else {
			// Go sets no Content-Type unless asked; make the absence explicit.
			req.Header.Del("Content-Type")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %q: %v", ct, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("content type %q: status %d, want %d",
				ct, resp.StatusCode, http.StatusUnsupportedMediaType)
		}
	}
}

// TestServeHTTPUncompressedContentType pins that the other §4 media type is
// still accepted — the rejection above is of unrecognised types, not of
// uncompressed framing.
func TestServeHTTPUncompressedContentType(t *testing.T) {
	r := setupSyncTestRepo(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Post(fmt.Sprintf("http://%s/", addr),
		xfer.ContentTypeUncompressed+"; charset=utf-8",
		strings.NewReader("pragma client-version 1\n"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("uncompressed POST status: %d, want 200", resp.StatusCode)
	}
}

func TestServeHTTPHandlerError(t *testing.T) {
	r := setupSyncTestRepo(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handler that always returns an error → server sends 500.
	failHandler := func(_ context.Context, _ *repo.Repo, _ *xfer.Message) (*xfer.Message, error) {
		return nil, fmt.Errorf("intentional handler failure")
	}

	go ServeHTTP(ctx, addr, r, failHandler)
	time.Sleep(100 * time.Millisecond)

	// Use raw HTTP to check the 500 status directly.
	body, _ := (&xfer.Message{Cards: []xfer.Card{
		&xfer.PullCard{ServerCode: "test", ProjectCode: "test"},
	}}).Encode()
	resp, err := http.Post(
		fmt.Sprintf("http://%s/", addr),
		"application/x-fossil",
		strings.NewReader(string(body)),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("handler error status: %d, want 500", resp.StatusCode)
	}
}

func TestServeHTTPClone(t *testing.T) {
	r := setupSyncTestRepo(t)
	stored := map[string]bool{}
	for i := 0; i < 3; i++ {
		data := []byte(fmt.Sprintf("clone http %d", i))
		uuid := hash.SHA1(data)
		storeReceivedFile(r, uuid, "", data, nil)
		stored[uuid] = true
	}

	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	transport := &HTTPTransport{URL: fmt.Sprintf("http://%s", addr)}
	req := &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1},
	}}
	resp, err := transport.Exchange(ctx, req)
	if err != nil {
		t.Fatalf("clone exchange: %v", err)
	}

	files := findCards[*xfer.CFileCard](resp)
	for _, f := range files {
		delete(stored, f.UUID)
	}
	if len(stored) > 0 {
		t.Fatalf("clone missing blobs: %v", stored)
	}
}

// storeLargeCloneCorpus fills r with incompressible blobs whose combined size
// forces the xfer clone response well past both Go's ~2KB response buffer (the
// point where net/http silently switches to chunked framing) and the ~256KB
// ceiling from issue #101. Random bytes are used so zlib cannot shrink the
// encoded response back under those thresholds. Returns the total raw bytes.
func storeLargeCloneCorpus(t *testing.T, r *repo.Repo, totalBytes int) int {
	t.Helper()
	if totalBytes <= 0 {
		t.Fatalf("storeLargeCloneCorpus: totalBytes must be positive, got %d", totalBytes)
	}
	rng := rand.New(rand.NewSource(101)) // Fixed seed: deterministic corpus.
	const blobBytes = 64 * 1024
	stored := 0
	for stored < totalBytes {
		data := make([]byte, blobBytes)
		if _, err := rng.Read(data); err != nil {
			t.Fatalf("rng read: %v", err)
		}
		uuid := hash.SHA1(data)
		storeReceivedFile(r, uuid, "", data, nil)
		stored += blobBytes
	}
	if stored <= 256*1024 {
		t.Fatalf("storeLargeCloneCorpus: corpus %d bytes not above 256KB", stored)
	}
	return stored
}

// rawCloneExchange performs a clone POST with the standard library HTTP client
// and returns the unread response so the caller can inspect wire framing
// (Content-Length, Transfer-Encoding) before draining the body. The transport
// helpers hide framing behind Go's transparent chunked handling, so a raw
// request is required to observe what issue #101's fossil <=2.23 client sees.
func rawCloneExchange(t *testing.T, addr string) *http.Response {
	t.Helper()
	body, err := (&xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1},
	}}).Encode()
	if err != nil {
		t.Fatalf("encode clone request: %v", err)
	}
	resp, err := http.Post(
		fmt.Sprintf("http://%s/", addr),
		xfer.ContentTypeCompressed,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("clone POST: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("clone status: %d, want 200", resp.StatusCode)
	}
	return resp
}

// TestServeHTTPCloneSetsContentLength reconstructs the failure mode of issue
// #101: release-tagged fossil <=2.23 reads the reply length only from the
// Content-Length header (src/http.c iLength stays negative otherwise) and
// aborts with "server did not reply" for any response above ~256KB, because
// net/http frames such large buffered responses as Transfer-Encoding: chunked.
//
// The reconstruction asserts the two properties a 2.23 client requires: a
// non-negative Content-Length (Go reports -1 for chunked/unknown, exactly the
// negative iLength that trips the abort) and the absence of chunked framing.
func TestServeHTTPCloneSetsContentLength(t *testing.T) {
	r := setupSyncTestRepo(t)
	corpus := storeLargeCloneCorpus(t, r, 512*1024)

	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	resp := rawCloneExchange(t, addr)
	defer resp.Body.Close()

	// A 2.23 client requires an explicit length. Go surfaces the header value
	// in resp.ContentLength and reports -1 when the server used chunked framing.
	if resp.ContentLength < 0 {
		t.Fatalf("response has no Content-Length (got %d) for a %d-byte corpus; "+
			"fossil <=2.23 aborts with \"server did not reply\"",
			resp.ContentLength, corpus)
	}
	for _, te := range resp.TransferEncoding {
		if te == "chunked" {
			t.Fatalf("response used Transfer-Encoding: chunked; "+
				"fossil <=2.23 cannot read a chunked reply (corpus %d bytes)", corpus)
		}
	}

	// No regression: the advertised length must match the delivered body, and
	// the body must still decode into the clone's stored-content cards.
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if int64(len(payload)) != resp.ContentLength {
		t.Fatalf("Content-Length %d != body %d", resp.ContentLength, len(payload))
	}
	msg, err := xfer.Decode(payload, resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("decode clone response: %v", err)
	}
	if len(findCards[*xfer.CFileCard](msg)) == 0 {
		t.Fatal("clone response carried no cfile cards")
	}
}

// TestServeHTTPModernCloneUnaffected pins that a modern client using Go's
// transparent HTTP handling still clones the same large corpus successfully,
// so the Content-Length framing is no regression for fossil 2.28 / libfossil
// peers that already tolerate either framing.
func TestServeHTTPModernCloneUnaffected(t *testing.T) {
	r := setupSyncTestRepo(t)
	storeLargeCloneCorpus(t, r, 512*1024)

	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ServeHTTP(ctx, addr, r, HandleSync)
	time.Sleep(100 * time.Millisecond)

	transport := &HTTPTransport{URL: fmt.Sprintf("http://%s", addr)}
	resp, err := transport.Exchange(ctx, &xfer.Message{Cards: []xfer.Card{
		&xfer.CloneCard{Version: 1},
	}})
	if err != nil {
		t.Fatalf("modern clone exchange: %v", err)
	}
	if len(findCards[*xfer.CFileCard](resp)) == 0 {
		t.Fatal("modern clone response carried no cfile cards")
	}
}
