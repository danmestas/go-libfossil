package conformance

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestZitHorizonNodesInterop runs a path cone's node elision under either
// horizon (draft-fossil-horizon-00 at zit 3a2df62515, §4.1, §5.1, §6.2)
// between go-libfossil and zit both ways: zit clones and pulls under cone
// src at a content and at a structural horizon from go-libfossil's handler,
// and go-libfossil clones and pulls the same from `zit serve`. Each origin
// sends exactly the nodes on paths into the cone of the trees its horizon
// keeps and announces none of the rest, and each client verifies clean.
func TestZitHorizonNodesInterop(t *testing.T) {
	bin := zitHorizonBinary(t)
	for _, depth := range []cone.HorizonDepth{cone.DepthContent, cone.DepthStructural} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			hzZitNodes(t, bin, depth)
		})
	}
}

func hzZitNodes(t *testing.T, bin string, depth cone.HorizonDepth) {
	o := newHZNodesOrigin(t)
	sc := o.cone(t, depth)
	c := hzUUID(t, o.f.r, o.c2)
	all := hzNodes(t, o.f.r)
	trees := []libfossil.FslID{o.c1, o.c2, o.c3}
	if depth == cone.DepthStructural {
		trees = trees[1:]
	}
	want := hzSlotNodes(t, o.f.r, sc, trees...)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	origin := &hzRecorder{r: o.f.r}
	server := httptest.NewServer(origin)
	defer server.Close()
	flags := []string{"--no-open", "--once", "--cone=src", "--horizon=" + c}
	if depth == cone.DepthStructural {
		flags = append(flags, "--structural")
	}

	// zit clones under the cone at the horizon from go-libfossil.
	zh := filepath.Join(dir, "zhorizon")
	if out, err := zitCommand(bin, home, dir, append([]string{"clone", server.URL, zh}, flags...)...); err != nil ||
		strings.Contains(out, "content horizon instead") {
		t.Fatalf("zit's clone from go-libfossil: %v\n%s", err, out)
	}
	if err := hzZitNodesServed(origin.take(), all, want); err != nil {
		t.Fatalf("go-libfossil serving zit's clone: %v", err)
	}
	zitRun(t, bin, home, zh, "verify")

	// zit pulls a check-in whose tree changes inside and outside the cone.
	zd := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", server.URL, zd, "--no-open", "--once")
	origin.take()
	files := scSyncTreeFiles()
	files["src/a"], files["zzz/b"], files["yyy/e/f"] = "inside 4\n", "outside 4\n", "yf 4\n"
	c4 := o.f.commit(t, files, o.c3)
	all = hzNodes(t, o.f.r)
	want = hzUnion(want, hzSlotNodes(t, o.f.r, sc, c4))
	zitRun(t, bin, home, zh, "pull", server.URL)
	if err := hzZitNodesServed(origin.take(), all, want); err != nil {
		t.Fatalf("go-libfossil serving zit's pull: %v", err)
	}
	zitRun(t, bin, home, zh, "verify")

	// go-libfossil clones and pulls under the cone at the horizon from zit.
	zitRun(t, bin, home, zd, "pull", server.URL)
	origin.take()
	peer := &hzRecordingTransport{inner: &sync.HTTPTransport{URL: zitServe(t, bin, home, zd)}}
	r, cloned, err := sync.Clone(context.Background(), filepath.Join(dir, "horizon.fossil"), peer,
		sync.CloneOpts{StoreCone: sc})
	if err != nil {
		t.Fatalf("go-libfossil's clone from zit: %v", err)
	}
	defer r.Close()
	if cloned.HorizonServed == 0 || depth == cone.DepthStructural && cloned.StructuralServed == 0 {
		t.Fatalf("zit served %d of %d rounds at the horizon, %d structural",
			cloned.HorizonServed, cloned.Rounds, cloned.StructuralServed)
	}
	if err := hzZitNodesSent(peer.replies, all, want); err != nil {
		t.Fatalf("zit serving go-libfossil's clone: %v", err)
	}
	files["src/shared"], files["zzz/deep/c"] = "shared 5\n", "deep 5\n"
	c5 := o.f.commit(t, files, c4)
	all = hzNodes(t, o.f.r)
	want = hzUnion(want, hzSlotNodes(t, o.f.r, sc, c5))
	zitRun(t, bin, home, zd, "pull", server.URL)
	peer.replies = nil
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil || len(pulled.Errors) != 0 {
		t.Fatalf("go-libfossil's pull from zit: %v %q", err, pulled.Errors)
	}
	if err := hzCrossed(peer.replies, hzMinusSet(all, want)); err != nil {
		t.Fatalf("zit serving go-libfossil's pull: %v", err)
	}
	if err := hzHeld(r, hzSlotNodes(t, o.f.r, sc, c5), true); err != nil {
		t.Fatal(err)
	}
	if err := hzNodesClean(r, hzMinusSet(all, want)); err != nil {
		t.Fatal(err)
	}
}

// hzZitNodesServed checks go-libfossil's replies to zit: every request
// carries the path member and the horizon, and the replies send only nodes
// of want and announce none outside it.
func hzZitNodesServed(exchanges []hzExchange, all, want map[string]bool) error {
	var replies []*xfer.Message
	for i, e := range exchanges {
		if len(hzPragmas(e.request, "horizon")) != 1 || len(hzPragmas(e.request, "cone")) != 1 {
			return expectf("request %d carries horizon %q and cone %q", i,
				hzPragmas(e.request, "horizon"), hzPragmas(e.request, "cone"))
		}
		replies = append(replies, e.reply)
	}
	if len(replies) == 0 {
		return expectf("no exchange")
	}
	return hzZitNodesSent(replies, all, want)
}

// hzZitNodesSent checks replies send or announce no node outside want.
func hzZitNodesSent(replies []*xfer.Message, all, want map[string]bool) error {
	if err := hzCrossed(replies, hzMinusSet(all, want)); err != nil {
		return err
	}
	if sent := hzIntersect(scSetOf(hzSent(replies)), all); len(sent) == 0 {
		return expectf("no node was sent")
	}
	return nil
}
