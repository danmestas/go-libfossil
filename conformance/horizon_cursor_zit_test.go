package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestZitHorizonCursorInterop runs the horizon cursor between go-libfossil
// and zit both ways (draft-fossil-horizon-00 §6.2.6): zit pulls at a horizon
// from go-libfossil's handler, records the cursor its reply hands out and
// sends it back, announcing nothing, and is announced only what lies above
// it; go-libfossil does the same against `zit serve`.
func TestZitHorizonCursorInterop(t *testing.T) {
	bin := zitHorizonBinary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	g := newHZSyncGraph(t)
	c := hzUUID(t, g.f.r, g.c3)
	origin := &hzRecorder{r: g.f.r}
	server := httptest.NewServer(origin)
	defer server.Close()

	// zit's first pull after its horizon clone is handed a cursor.
	zhorizon := filepath.Join(dir, "zhorizon")
	zitRun(t, bin, home, dir, "clone", "--no-open", "--horizon="+c, server.URL, zhorizon)
	zitRun(t, bin, home, zhorizon, "pull")
	n, err := hzLastCursor(hzReplies(origin.take()))
	if err != nil {
		t.Fatalf("go-libfossil's reply to zit's pull: %v", err)
	}

	// Its next pull sends the cursor, announces nothing, and is announced
	// only what the origin appended since.
	hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	zitRun(t, bin, home, zhorizon, "pull")
	exchanges := origin.take()
	if err := hzZitCursorPull(exchanges, n); err != nil {
		t.Fatalf("zit's pull under go-libfossil's cursor: %v", err)
	}
	for _, e := range exchanges {
		above, err := hzAbove(g.f.r, hzIGots(e.reply), n)
		if err != nil {
			t.Fatal(err)
		}
		if len(above) != len(hzIGots(e.reply)) {
			t.Fatalf("go-libfossil announced %d artifacts at or below cursor %d to zit",
				len(hzIGots(e.reply))-len(above), n)
		}
	}
	if len(hzAnnounced(hzReplies(exchanges))) == 0 {
		t.Fatal("go-libfossil announced none of what it appended above zit's cursor")
	}

	// go-libfossil clones at the horizon from zit, and its first pull is
	// handed zit's cursor.
	zdense := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", "--no-open", server.URL, zdense)
	zitURL := zitServe(t, bin, home, zdense)
	peer := &hzRecordingTransport{inner: &sync.HTTPTransport{URL: zitURL}}
	r, _, err := sync.Clone(context.Background(), filepath.Join(dir, "horizon.fossil"), peer,
		sync.CloneOpts{StoreCone: g.c, PullPhantoms: true})
	if err != nil {
		t.Fatalf("go-libfossil's horizon clone from zit: %v", err)
	}
	defer r.Close()
	opts := sync.SyncOpts{Pull: true, Origin: zitURL}
	peer.replies = nil
	if _, err := sync.Sync(context.Background(), r, peer, opts); err != nil {
		t.Fatalf("go-libfossil's pull from zit: %v", err)
	}
	zn, err := hzLastCursor(peer.replies)
	if err != nil {
		t.Fatalf("zit's reply to go-libfossil's pull: %v", err)
	}

	// Its next pull sends zit's cursor and announces nothing; zit announces
	// only what it appended since, which go-libfossil then holds.
	above := hzCommit(t, g.f, "c6", []libfossil.FslID{g.m}, nil)
	zitRun(t, bin, home, zdense, "pull", server.URL)
	sent := &hzSentTransport{inner: peer}
	peer.replies = nil
	pulled, err := sync.Sync(context.Background(), r, sent, opts)
	if err != nil {
		t.Fatalf("go-libfossil's pull from zit under its cursor: %v", err)
	}
	if err := hzRequestCursors(sent.requests, zn); err != nil {
		t.Fatalf("go-libfossil's pull from zit: %v", err)
	}
	if got := hzAnnounced(sent.requests); len(got) != 0 {
		t.Fatalf("go-libfossil's pull-only request under zit's cursor announced %d artifacts", len(got))
	}
	if len(pulled.Errors) != 0 {
		t.Fatalf("pull from zit: %q", pulled.Errors)
	}
	want := hzFiles(t, g.f.r, above)
	want[hzUUID(t, g.f.r, above)] = true
	got := hzAnnounced(peer.replies)
	if len(got) == 0 {
		t.Fatal("zit announced nothing it appended above its cursor")
	}
	for uuid := range got {
		if !want[uuid] {
			t.Fatalf("zit announced %s, below its cursor", uuid)
		}
	}
	if err := hzHeld(r, want, true); err != nil {
		t.Fatal(err)
	}
}

// hzReplies is every reply of exchanges.
func hzReplies(exchanges []hzExchange) []*xfer.Message {
	out := make([]*xfer.Message, 0, len(exchanges))
	for _, e := range exchanges {
		out = append(out, e.reply)
	}
	return out
}

// hzZitCursorPull fails unless every request zit sent carries cursor n and
// no igot.
func hzZitCursorPull(exchanges []hzExchange, n int64) error {
	if len(exchanges) == 0 {
		return expectf("zit sent no request")
	}
	requests := make([]*xfer.Message, 0, len(exchanges))
	for _, e := range exchanges {
		requests = append(requests, e.request)
	}
	if err := hzRequestCursors(requests, n); err != nil {
		return err
	}
	if got := hzAnnounced(requests); len(got) != 0 {
		return expectf("zit's pull-only requests announced %d artifacts", len(got))
	}
	return nil
}

// hzSentTransport records the requests sent through it.
type hzSentTransport struct {
	inner    sync.Transport
	requests []*xfer.Message
}

func (p *hzSentTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	wire, err := scSyncWire(req)
	if err != nil {
		return nil, err
	}
	p.requests = append(p.requests, wire)
	return p.inner.Exchange(ctx, req)
}

// TestZitHorizonCloneCursorInterop runs the cursor of a clone between
// go-libfossil and zit both ways (draft-fossil-horizon-00 §6.2.6): zit's
// horizon clone from go-libfossil's handler is handed the cursor in its
// clone replies, so its first pull sends it and is announced nothing;
// go-libfossil's horizon clone from `zit serve` records zit's, so its first
// pull sends it and announces nothing.
func TestZitHorizonCloneCursorInterop(t *testing.T) {
	bin := zitHorizonBinary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	g := newHZSyncGraph(t)
	c := hzUUID(t, g.f.r, g.c3)
	origin := &hzRecorder{r: g.f.r}
	server := httptest.NewServer(origin)
	defer server.Close()

	zhorizon := filepath.Join(dir, "zhorizon")
	zitRun(t, bin, home, dir, "clone", "--no-open", "--horizon="+c, server.URL, zhorizon)
	n, err := hzLastCursor(hzReplies(origin.take()))
	if err != nil {
		t.Fatalf("go-libfossil's clone replies to zit: %v", err)
	}
	if top, err := hzMaxRID(g.f.r); err != nil || n != top {
		t.Fatalf("clone cursor %d, want the origin's position %d (%v)", n, top, err)
	}
	zitRun(t, bin, home, zhorizon, "pull")
	if err := hzZitCursorPull(origin.take(), n); err != nil {
		t.Fatalf("zit's first pull after its clone: %v", err)
	}

	zdense := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", "--no-open", server.URL, zdense)
	zitURL := zitServe(t, bin, home, zdense)
	peer := &hzRecordingTransport{inner: &sync.HTTPTransport{URL: zitURL}}
	r, _, err := sync.Clone(context.Background(), filepath.Join(dir, "horizon.fossil"), peer,
		sync.CloneOpts{StoreCone: g.c, PullPhantoms: true, Origin: zitURL})
	if err != nil {
		t.Fatalf("go-libfossil's horizon clone from zit: %v", err)
	}
	defer r.Close()
	zn, err := hzLastCursor(peer.replies)
	if err != nil {
		t.Fatalf("zit's clone replies to go-libfossil: %v", err)
	}
	sent := &hzSentTransport{inner: peer}
	pulled, err := sync.Sync(context.Background(), r, sent, sync.SyncOpts{Pull: true, Origin: zitURL})
	if err != nil {
		t.Fatalf("go-libfossil's first pull from zit: %v", err)
	}
	if len(pulled.Errors) != 0 {
		t.Fatalf("pull from zit: %q", pulled.Errors)
	}
	if err := hzRequestCursors(sent.requests, zn); err != nil {
		t.Fatalf("go-libfossil's first pull after its clone: %v", err)
	}
	if got := hzAnnounced(sent.requests); len(got) != 0 {
		t.Fatalf("go-libfossil's first pull after its clone announced %d artifacts", len(got))
	}
}
