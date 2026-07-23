package sync

import (
	"context"
	"fmt"
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

	files := findCards[*xfer.FileCard](resp)
	for _, f := range files {
		delete(stored, f.UUID)
	}
	if len(stored) > 0 {
		t.Fatalf("clone missing blobs: %v", stored)
	}
}
