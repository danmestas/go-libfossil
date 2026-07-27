package sync_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// flakyProxy fronts a real xfer server and severs the connection part-way
// through the replies whose request ordinal is in dropAt. The drop is injected
// by hijacking the connection, announcing a full-length reply, writing half of
// it and closing — which is what a peer that goes away mid-exchange looks like
// from the client's side, and is the failure shape reported in #185
// ("http transport: Post ...: EOF" with no store produced).
//
// Ordinals count every request the proxy sees, retries included, so a test can
// place a drop before and after content has been received.
type flakyProxy struct {
	upstream string
	dropAt   map[int]bool

	mu       gosync.Mutex
	requests int
	dropped  int
}

func (p *flakyProxy) counts() (requests, dropped int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests, p.dropped
}

func (p *flakyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests++
	ordinal := p.requests
	drop := p.dropAt[ordinal]
	if drop {
		p.dropped++
	}
	p.mu.Unlock()

	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := http.Post(p.upstream, r.Header.Get("Content-Type"), bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	ct := resp.Header.Get("Content-Type")

	if !drop {
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(respBody); err != nil {
			panic("flakyProxy: write reply: " + err.Error())
		}
		return
	}

	// Sever mid-reply: promise the whole body, deliver half of it, close.
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("flakyProxy: ResponseWriter is not a Hijacker")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		panic("flakyProxy: hijack: " + err.Error())
	}
	defer conn.Close()
	fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", ct, len(respBody))
	if _, err := buf.Write(respBody[:len(respBody)/2]); err != nil {
		panic("flakyProxy: write truncated reply: " + err.Error())
	}
	if err := buf.Flush(); err != nil {
		panic("flakyProxy: flush truncated reply: " + err.Error())
	}
}

// newRetryFixtureRepo builds a small source repository whose clone carries
// several artifacts, so a truncated reply discards real content.
func newRetryFixtureRepo(t *testing.T, dir string) *repo.Repo {
	t.Helper()
	src := newSelfRoundTripRepo(t, dir)
	for c := 0; c < 3; c++ {
		if _, _, err := manifest.Checkin(src, manifest.CheckinOpts{
			Comment: fmt.Sprintf("checkin %d", c),
			User:    "testuser",
			Files: []manifest.File{
				{Name: fmt.Sprintf("text%d.txt", c), Content: []byte(fmt.Sprintf("checkin %d\n", c))},
				{Name: fmt.Sprintf("data%d.bin", c), Content: incompressibleBytes(int64(c), 32<<10)},
			},
		}); err != nil {
			src.Close()
			t.Fatalf("Checkin %d: %v", c, err)
		}
	}
	return src
}

// TestCloneSurvivesMidExchangeDisconnect is the #185 regression test: a clone
// whose connection is severed part-way through a reply must retry that round
// and still produce a complete store, rather than aborting the whole exchange.
//
// Method: a real xfer server behind a proxy that truncates the first reply and
// a later one. Two drops, at different points in the exchange, so the fix is
// shown to survive a drop both before and after content has landed.
func TestCloneSurvivesMidExchangeDisconnect(t *testing.T) {
	dir := t.TempDir()
	srcRepo := newRetryFixtureRepo(t, dir)
	defer srcRepo.Close()

	want := blobUUIDs(t, srcRepo)
	if len(want) == 0 {
		t.Fatal("source repository holds no blobs")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := serveRepo(ctx, t, srcRepo)

	proxy := &flakyProxy{upstream: upstream, dropAt: map[int]bool{1: true, 3: true}}
	front := httptest.NewServer(proxy)
	defer front.Close()

	clonePath := filepath.Join(dir, "clone.fossil")
	cloneRepo, result, err := sync.Clone(
		ctx, clonePath, &sync.HTTPTransport{URL: front.URL}, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("Clone through a connection that drops mid-exchange: %v", err)
	}
	defer cloneRepo.Close()

	requests, dropped := proxy.counts()
	if dropped != 2 {
		t.Fatalf("proxy dropped %d replies, want 2 (the clone made %d requests over %d rounds)",
			dropped, requests, result.Rounds)
	}
	if requests <= result.Rounds {
		t.Errorf("proxy saw %d requests for %d rounds; retries should have added requests",
			requests, result.Rounds)
	}

	got := blobUUIDs(t, cloneRepo)
	var missing []string
	for uuid := range want {
		if !got[uuid] {
			missing = append(missing, uuid)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("clone is missing %d of %d artifacts: %v", len(missing), len(want), missing)
	}
}

// TestCloneUnreachableServerFailsFast requires a dead peer to end the clone
// promptly and say how many attempts it made, rather than retrying forever.
func TestCloneUnreachableServerFailsFast(t *testing.T) {
	// A port that was bound and released: connections are refused, not black-holed.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	clonePath := filepath.Join(t.TempDir(), "clone.fossil")
	start := time.Now()
	_, _, cloneErr := sync.Clone(context.Background(), clonePath,
		&sync.HTTPTransport{URL: "http://" + addr}, sync.CloneOpts{})
	elapsed := time.Since(start)

	if cloneErr == nil {
		t.Fatal("Clone against a refused port returned nil error")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("Clone against a refused port took %v, want a prompt bounded failure", elapsed)
	}
	if !strings.Contains(cloneErr.Error(), "attempt") {
		t.Errorf("error does not name the attempts made: %v", cloneErr)
	}
}

// alwaysServer replies with a fixed status, content type and body, counting
// requests. It stands in for a peer whose replies are wrong rather than absent.
type alwaysServer struct {
	status      int
	contentType string
	body        []byte

	mu       gosync.Mutex
	requests int
}

func (s *alwaysServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *alwaysServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		panic("alwaysServer: drain request: " + err.Error())
	}
	w.Header().Set("Content-Type", s.contentType)
	w.WriteHeader(s.status)
	if _, err := w.Write(s.body); err != nil {
		panic("alwaysServer: write reply: " + err.Error())
	}
}

// TestCloneProtocolErrorFailsWithoutRetry requires a reply that arrived intact
// but does not parse to abort on its first occurrence. Retrying a malformed
// reply would hide the defect that produced it, so the classification has to
// discriminate this from a severed connection.
func TestCloneProtocolErrorFailsWithoutRetry(t *testing.T) {
	// "pull" takes two arguments; one is a parse error, not a transport fault.
	peer := &alwaysServer{status: http.StatusOK, contentType: xfer.ContentTypeUncompressed, body: []byte("pull onearg\n")}
	front := httptest.NewServer(peer)
	defer front.Close()

	clonePath := filepath.Join(t.TempDir(), "clone.fossil")
	_, _, err := sync.Clone(context.Background(), clonePath,
		&sync.HTTPTransport{URL: front.URL}, sync.CloneOpts{})
	if err == nil {
		t.Fatal("Clone against a peer replying with an unparsable body returned nil error")
	}
	if got := peer.count(); got != 1 {
		t.Errorf("peer saw %d requests, want 1: a malformed reply must not be retried", got)
	}
}

// TestCloneClientErrorStatusFailsWithoutRetry requires a 4xx — a request the
// peer will reject however many times it is repeated — to fail immediately.
func TestCloneClientErrorStatusFailsWithoutRetry(t *testing.T) {
	peer := &alwaysServer{status: http.StatusForbidden, contentType: "text/plain", body: []byte("no")}
	front := httptest.NewServer(peer)
	defer front.Close()

	clonePath := filepath.Join(t.TempDir(), "clone.fossil")
	_, _, err := sync.Clone(context.Background(), clonePath,
		&sync.HTTPTransport{URL: front.URL}, sync.CloneOpts{})
	if err == nil {
		t.Fatal("Clone against a peer replying 403 returned nil error")
	}
	if got := peer.count(); got != 1 {
		t.Errorf("peer saw %d requests, want 1: a 4xx must not be retried", got)
	}
}

// TestCloneServerErrorStatusIsRetried requires a 5xx — the shape
// fossil-scm.org returned to CI runners in #173 — to be retried and then to
// give up with a bounded, named failure.
func TestCloneServerErrorStatusIsRetried(t *testing.T) {
	peer := &alwaysServer{status: http.StatusServiceUnavailable, contentType: "text/plain", body: []byte("busy")}
	front := httptest.NewServer(peer)
	defer front.Close()

	clonePath := filepath.Join(t.TempDir(), "clone.fossil")
	_, _, err := sync.Clone(context.Background(), clonePath,
		&sync.HTTPTransport{URL: front.URL}, sync.CloneOpts{})
	if err == nil {
		t.Fatal("Clone against a peer replying 503 returned nil error")
	}
	if got, want := peer.count(), sync.MaxExchangeAttempts; got != want {
		t.Errorf("peer saw %d requests, want %d: a 5xx should be retried up to the bound", got, want)
	}
}

// TestCloneRetryStopsOnCancel requires a cancelled context to abort during the
// backoff wait rather than sleeping it out, preserving the deadline
// propagation this repository already relies on.
func TestCloneRetryStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := make(chan struct{}, 1)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- struct{}{}:
		default:
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("hijack unsupported")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			panic("hijack: " + err.Error())
		}
		conn.Close()
	}))
	defer front.Close()

	go func() {
		<-seen
		cancel()
	}()

	clonePath := filepath.Join(t.TempDir(), "clone.fossil")
	start := time.Now()
	_, _, err := sync.Clone(ctx, clonePath, &sync.HTTPTransport{URL: front.URL}, sync.CloneOpts{})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Clone error = %v, want context.Canceled", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("cancelled clone took %v; the backoff wait should have aborted", elapsed)
	}
}

// TestCloneRetryHonoursDeadline requires retries to stop at the caller's
// deadline instead of running past it.
func TestCloneRetryHonoursDeadline(t *testing.T) {
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("hijack unsupported")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			panic("hijack: " + err.Error())
		}
		conn.Close()
	}))
	defer front.Close()

	deadline := 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	clonePath := filepath.Join(t.TempDir(), "clone.fossil")
	start := time.Now()
	_, _, err := sync.Clone(ctx, clonePath, &sync.HTTPTransport{URL: front.URL}, sync.CloneOpts{})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Clone error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("clone ran %v past a %v deadline", elapsed, deadline)
	}
}
