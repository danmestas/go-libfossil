package conformance

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestZitHorizonInterop runs the horizon pragmas between go-libfossil and
// zit both ways (draft-fossil-horizon-00 §6.1, §6.2, §6.2.5): zit clones
// and pulls at a horizon from go-libfossil's handler, and go-libfossil
// clones and pulls at the same horizon from `zit serve`. Each origin sends
// no blob below the horizon and says horizon-served content; each client
// holds the blobs above it and records the rest elided.
func TestZitHorizonInterop(t *testing.T) {
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

	// zit clones at the horizon from go-libfossil.
	zhorizon := filepath.Join(dir, "zhorizon")
	zitRun(t, bin, home, dir, "clone", "--no-open", "--horizon="+c, server.URL, zhorizon)
	if err := hzZitServed(origin.take(), c, g.below); err != nil {
		t.Fatalf("zit's horizon clone from go-libfossil: %v", err)
	}
	if got := zitElidedCount(t, zitRun(t, bin, home, zhorizon, "verify")); got != len(g.below) {
		t.Fatalf("zit's horizon clone elided %d, want %d", got, len(g.below))
	}

	// zit pulls a check-in above the horizon and one below it.
	zdense := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", "--no-open", server.URL, zdense)
	origin.take()
	above := hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	below := hzCommit(t, g.f, "feat2", []libfossil.FslID{g.feat}, nil)
	newAbove := hzMinus(hzFiles(t, g.f.r, above), g.above)
	newBelow := hzMinus(hzFiles(t, g.f.r, below), hzFiles(t, g.f.r, g.checkins()...))
	if len(newAbove) != 1 || len(newBelow) != 1 {
		t.Fatalf("fixture: new blobs above %v below %v", newAbove, newBelow)
	}
	elided := hzUnion(g.below, map[string]bool{newBelow[0]: true})
	zitRun(t, bin, home, zhorizon, "pull")
	if err := hzZitServed(origin.take(), c, elided); err != nil {
		t.Fatalf("zit's horizon pull from go-libfossil: %v", err)
	}
	if got := zitElidedCount(t, zitRun(t, bin, home, zhorizon, "verify")); got != len(elided) {
		t.Fatalf("zit's horizon pull elided %d, want %d", got, len(elided))
	}

	// go-libfossil clones and pulls at the horizon from zit.
	zitURL := zitServe(t, bin, home, zdense)
	peer := &hzRecordingTransport{inner: &sync.HTTPTransport{URL: zitURL}}
	r, cloned, err := sync.Clone(context.Background(), filepath.Join(dir, "horizon.fossil"), peer,
		sync.CloneOpts{StoreCone: g.c, PullPhantoms: true})
	if err != nil {
		t.Fatalf("go-libfossil's horizon clone from zit: %v", err)
	}
	defer r.Close()
	if cloned.HorizonServed == 0 {
		t.Fatalf("zit served none of %d clone rounds at the horizon", cloned.Rounds)
	}
	if err := hzCrossed(peer.replies, g.below); err != nil {
		t.Fatalf("zit's horizon clone reply: %v", err)
	}
	if err := hzHeld(r, g.above, true); err != nil {
		t.Fatal(err)
	}
	if err := hzElided(r, g.below); err != nil {
		t.Fatal(err)
	}
	zitRun(t, bin, home, zdense, "pull", server.URL)
	peer.replies = nil
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		t.Fatalf("go-libfossil's horizon pull from zit: %v", err)
	}
	if pulled.HorizonServed == 0 || len(pulled.Errors) != 0 {
		t.Fatalf("pull from zit: served %d of %d rounds, errors %q", pulled.HorizonServed, pulled.Rounds, pulled.Errors)
	}
	if err := hzCrossed(peer.replies, elided); err != nil {
		t.Fatalf("zit's horizon pull reply: %v", err)
	}
	if err := hzHeld(r, map[string]bool{newAbove[0]: true}, true); err != nil {
		t.Fatal(err)
	}
	if err := hzElided(r, elided); err != nil {
		t.Fatal(err)
	}

	// Each horizon store refuses the other's dense clone (§6.4), and the
	// refused client fails, while a clone at the same horizon is served.
	zhorizonURL := zitServe(t, bin, home, zhorizon)
	if _, _, err := sync.Clone(context.Background(), filepath.Join(dir, "refused.fossil"),
		&sync.HTTPTransport{URL: zhorizonURL}, sync.CloneOpts{}); err == nil {
		t.Fatal("go-libfossil's dense clone of zit's horizon store succeeded")
	}
	relay, _, err := sync.Clone(context.Background(), filepath.Join(dir, "relay.fossil"),
		&sync.HTTPTransport{URL: zhorizonURL}, sync.CloneOpts{StoreCone: g.c})
	if err != nil {
		t.Fatalf("go-libfossil's horizon clone of zit's horizon store: %v", err)
	}
	relay.Close()
	if _, err := r.DB().Exec("UPDATE user SET cap='gio' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	store := httptest.NewServer(sync.XferHandler(r, sync.HandleSync))
	defer store.Close()
	out, err := zitCommand(bin, home, dir, "clone", "--no-open", store.URL, filepath.Join(dir, "zrefused"))
	if err == nil || !strings.Contains(out, scSyncOriginRefusal) {
		t.Fatalf("zit's dense clone of go-libfossil's horizon store: %v\n%s", err, out)
	}
	zitRun(t, bin, home, dir, "clone", "--no-open", "--horizon="+c, store.URL, filepath.Join(dir, "zrelay"))
}

// zitCommand runs zit as zitRun does and returns its combined output and
// error instead of failing the test.
func zitCommand(bin, home, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "FOSSIL_HOME="+home, "USER=u")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// hzZitServed checks what zit asked and what go-libfossil answered: each
// request carrying the horizon pragma names C under the descendants rule,
// at least one does, each reply to one says horizon-served content, and no
// reply carries or announces a blob of below.
func hzZitServed(exchanges []hzExchange, c string, below map[string]bool) error {
	scoped := 0
	var replies []*xfer.Message
	for i, e := range exchanges {
		replies = append(replies, e.reply)
		got := hzPragmas(e.request, "horizon")
		if len(got) == 0 {
			continue
		}
		scoped++
		if len(got) != 1 || strings.Join(got[0], " ") != c+" descendants" {
			return expectf("request %d carries horizon pragmas %q", i, got)
		}
		if err := hzServedReplies([]*xfer.Message{e.reply}, 1); err != nil {
			return expectf("exchange %d: %v", i, err)
		}
	}
	if scoped == 0 {
		return expectf("no request of %d carried the horizon pragma", len(exchanges))
	}
	return hzCrossed(replies, below)
}

type hzExchange struct{ request, reply *xfer.Message }

// hzRecorder serves r over HTTP as go-libfossil's handler does and records
// each exchange decoded. While ignorant it serves as a horizon-ignorant peer
// does: the horizon pragmas are dropped from each request before serving
// (horizon §6.3), and the request is recorded as sent.
type hzRecorder struct {
	r         *repo.Repo
	mu        gosync.Mutex
	exchanges []hzExchange
	ignorant  bool
}

func (s *hzRecorder) setIgnorant(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ignorant = v
}

func (s *hzRecorder) take() []hzExchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.exchanges
	s.exchanges = nil
	return out
}

func (s *hzRecorder) ServeHTTP(w http.ResponseWriter, hr *http.Request) {
	body, err := io.ReadAll(hr.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, err := xfer.Decode(body, hr.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	served := req
	s.mu.Lock()
	if s.ignorant {
		served = &xfer.Message{}
		for _, c := range req.Cards {
			if p, ok := c.(*xfer.PragmaCard); ok && strings.HasPrefix(p.Name, "horizon") {
				continue
			}
			served.Cards = append(served.Cards, c)
		}
	}
	s.mu.Unlock()
	resp, err := sync.HandleSync(hr.Context(), s.r, served)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	encode, contentType := resp.Encode, xfer.ContentTypeCompressed
	if resp.Uncompressed {
		encode, contentType = resp.EncodeUncompressed, xfer.ContentTypeUncompressed
	}
	data, err := encode()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	decoded, err := xfer.Decode(bytes.Clone(data), contentType)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.exchanges = append(s.exchanges, hzExchange{request: req, reply: decoded})
	s.mu.Unlock()
	w.Header().Set("Content-Type", contentType)
	w.Write(data)
}

// hzRecordingTransport records the replies a transport returns.
type hzRecordingTransport struct {
	inner   sync.Transport
	replies []*xfer.Message
}

func (p *hzRecordingTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	resp, err := p.inner.Exchange(ctx, req)
	if err == nil {
		p.replies = append(p.replies, resp)
	}
	return resp, err
}
