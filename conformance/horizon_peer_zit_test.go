package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestZitHorizonPeer has zit pull from a go-libfossil horizon store
// (draft-fossil-horizon-00 §6.4): a pull at the store's own C is served at
// the horizon, and a pull at an earlier C, whose held set the store does not
// hold, is refused with the one error card, on which zit fails.
func TestZitHorizonPeer(t *testing.T) {
	bin := zitHorizonBinary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	g := newHZSyncGraph(t)
	_, store, _, err := hzServeClone(t, g, g.c, sync.HandleOpts{})
	if err != nil {
		t.Fatal(err)
	}
	hzPeerOpen(t, store)
	recorder := &hzRecorder{r: store}
	server := httptest.NewServer(recorder)
	defer server.Close()
	origin := httptest.NewServer(sync.XferHandler(g.f.r, sync.HandleSync))
	defer origin.Close()

	c := hzUUID(t, g.f.r, g.c3)
	same := filepath.Join(dir, "zsame")
	zitRun(t, bin, home, dir, "clone", "--no-open", "--horizon="+c, origin.URL, same)
	zitRun(t, bin, home, same, "pull", server.URL)
	if err := hzZitServed(recorder.take(), c, g.below); err != nil {
		t.Fatalf("zit's pull at the store's own C: %v", err)
	}

	earlier := filepath.Join(dir, "zearlier")
	zitRun(t, bin, home, dir, "clone", "--no-open", "--horizon="+hzUUID(t, g.f.r, g.c2), origin.URL, earlier)
	out, err := zitCommand(bin, home, earlier, "pull", server.URL)
	if err == nil {
		t.Fatalf("zit's pull at an earlier C succeeded, want the refusal as fatal\n%s", out)
	}
	exchanges := recorder.take()
	if len(exchanges) != 1 || len(hzPragmas(exchanges[0].request, "horizon")) != 1 {
		t.Fatalf("zit's refused pull made %d exchanges, want one horizon request", len(exchanges))
	}
	if err := hzPeerRefused(exchanges[0].reply); err != nil {
		t.Fatalf("zit's refused pull: %v", err)
	}
}

// TestZitHorizonPeerServes has go-libfossil pull at an earlier C from zit's
// horizon store. zit is the server: when it refuses with one error card and
// no content (§6.4) carrying the horizon-not-held: prefix, go-libfossil's
// pull must fail and store nothing. A zit that serves the pull instead, or
// refuses without the prefix, does not implement the refusal clients match,
// and the case is skipped.
func TestZitHorizonPeerServes(t *testing.T) {
	bin := zitHorizonBinary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	g := newHZSyncGraph(t)
	origin := httptest.NewServer(sync.XferHandler(g.f.r, sync.HandleSync))
	defer origin.Close()
	zstore := filepath.Join(dir, "zstore")
	zitRun(t, bin, home, dir, "clone", "--no-open", "--horizon="+hzUUID(t, g.f.r, g.c3), origin.URL, zstore)
	peer := &hzRecordingTransport{inner: &sync.HTTPTransport{URL: zitServe(t, bin, home, zstore)}}

	earlier := hzStoreCone(t, hzUUID(t, g.f.r, g.c2), cone.RuleDescendants, cone.DepthContent)
	_, r, _, err := hzServeClone(t, g, earlier, sync.HandleOpts{})
	if err != nil {
		t.Fatal(err)
	}
	before := hzBlobCount(t, r)
	_, err = sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	var refusal string
	for _, reply := range peer.replies {
		for _, card := range reply.Cards {
			if c, ok := card.(*xfer.ErrorCard); ok {
				refusal = c.Message
			}
		}
	}
	switch {
	case refusal == "":
		t.Skipf("zit (%s) serves a horizon pull at an earlier C than its own (§6.4 refusal not implemented)", zitVersion(bin))
	case !strings.HasPrefix(refusal, hzPeerRefusalPrefix):
		t.Skipf("zit (%s) refuses with %q, which lacks the %s prefix a client matches", zitVersion(bin), refusal,
			hzPeerRefusalPrefix)
	}
	if err == nil {
		t.Fatalf("go-libfossil's pull took zit's refusal %q as non-fatal", refusal)
	}
	if after := hzBlobCount(t, r); after != before {
		t.Fatalf("the refused pull went from %d blob rows to %d", before, after)
	}
}
