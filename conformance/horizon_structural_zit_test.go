package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestZitHorizonStructuralInterop runs the structural horizon between
// go-libfossil and zit both ways (draft-fossil-horizon-00 §5, §6.2.2,
// §6.2.3, §6.2.5) over hzSeed: zit clones and pulls at C's structural horizon
// from go-libfossil's handler, and go-libfossil clones and pulls at it from
// `zit serve`. Each origin sends no manifest below the horizon, labels the
// boundary and sends the seed; each client stays structural and derives the
// branch, leaf and tag state of K exactly as a dense clone of the same origin
// does.
func TestZitHorizonStructuralInterop(t *testing.T) {
	bin := zitHorizonBinary(t)
	x := newHZStruct(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	origin := &hzRecorder{r: x.s.f.r}
	server := httptest.NewServer(origin)
	defer server.Close()
	k := []string{x.c3, x.c4, x.feat2}

	// zit clones at the structural horizon from go-libfossil.
	zs := filepath.Join(dir, "zstructural")
	if out, err := zitCommand(bin, home, dir, "clone", server.URL, zs, "--no-open", "--once",
		"--horizon="+x.c3, "--structural"); err != nil || strings.Contains(out, "content horizon instead") {
		t.Fatalf("zit's structural clone from go-libfossil: %v\n%s", err, out)
	}
	if err := hzStructZitServed(origin.take(), x); err != nil {
		t.Fatalf("zit's structural clone from go-libfossil: %v", err)
	}
	_, want, err := x.s.mint(seedLoginGo)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(zs, ".zit-cache", "horizon-seed")); err != nil || string(got) != string(want) {
		t.Fatalf("zit holds seed %q (%v), want go-libfossil's:\n%s", got, err, want)
	}
	zd := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", server.URL, zd, "--no-open", "--once")
	origin.take()
	if err := hzZitSameState(t, bin, home, zs, zd, k); err != nil {
		t.Fatalf("zit's structural clone of go-libfossil: %v", err)
	}

	// A check-in above the horizon and a tag below it that moves the state
	// in force at C: zit's pull brings the check-in and the re-minted seed.
	c5rid := hzCommit(t, x.s.f, "c5", []libfossil.FslID{x.s.c4}, nil)
	c5 := hzUUID(t, x.s.f.r, c5rid)
	x.s.tag(t, x.s.c1, "hue", tag.TagPropagating, "blue")
	k = append(k, c5)
	zitRun(t, bin, home, zs, "pull", server.URL)
	if err := hzStructZitServed(origin.take(), x); err != nil {
		t.Fatalf("zit's structural pull from go-libfossil: %v", err)
	}
	zitRun(t, bin, home, zd, "pull", server.URL)
	origin.take()
	if err := hzZitSameState(t, bin, home, zs, zd, k); err != nil {
		t.Fatalf("zit's structural pull from go-libfossil: %v", err)
	}

	// go-libfossil clones and pulls at the structural horizon from zit.
	zitURL := zitServe(t, bin, home, zd)
	peer := &hzRecordingTransport{inner: &sync.HTTPTransport{URL: zitURL}}
	r, cloned, err := sync.Clone(context.Background(), filepath.Join(dir, "structural.fossil"), peer,
		sync.CloneOpts{StoreCone: x.cone(t), PullPhantoms: true})
	if err != nil {
		t.Fatalf("go-libfossil's structural clone from zit: %v", err)
	}
	defer r.Close()
	if cloned.HorizonFallback || cloned.StructuralServed == 0 {
		t.Fatalf("clone from zit fell back %v, structural in %d of %d rounds",
			cloned.HorizonFallback, cloned.StructuralServed, cloned.Rounds)
	}
	if err := hzCrossed(peer.replies, x.below()); err != nil {
		t.Fatalf("zit's structural clone reply: %v", err)
	}
	if held, _ := seed.Held(r.DB()); held == nil {
		t.Fatal("go-libfossil holds no seed from zit")
	}
	if err := x.s.derivesDense(t, r); err != nil {
		t.Fatalf("go-libfossil's structural clone of zit: %v", err)
	}
	if err := hzElided(r, x.below()); err != nil {
		t.Fatalf("go-libfossil's structural clone of zit: %v", err)
	}
	c6 := hzCommit(t, x.s.f, "c6", []libfossil.FslID{x.s.c3}, hzBranch("side"))
	x.s.tag(t, x.s.feat1, "closed", tag.TagCancel, "")
	zitRun(t, bin, home, zd, "pull", server.URL)
	peer.replies = nil
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil {
		t.Fatalf("go-libfossil's structural pull from zit: %v", err)
	}
	if pulled.HorizonFallback || pulled.StructuralServed == 0 || len(pulled.Errors) != 0 {
		t.Fatalf("pull from zit: fell back %v, structural %d of %d rounds, errors %q",
			pulled.HorizonFallback, pulled.StructuralServed, pulled.Rounds, pulled.Errors)
	}
	if err := hzCrossed(peer.replies, x.below()); err != nil {
		t.Fatalf("zit's structural pull reply: %v", err)
	}
	x.s.k = append(x.s.k, c5rid, c6)
	if err := x.s.derivesDense(t, r); err != nil {
		t.Fatalf("go-libfossil's structural pull from zit: %v", err)
	}
}

// seedLoginGo is the U card go-libfossil's origin mints seeds with.
const seedLoginGo = "origin"

// hzStructZitServed checks zit's structural requests and go-libfossil's
// replies: every horizon request carries horizon-structural and a seed offer,
// every reply to one says horizon-served structural, and nothing below the
// horizon is sent or announced.
func hzStructZitServed(exchanges []hzExchange, x *hzStruct) error {
	scoped := 0
	var replies []*xfer.Message
	for i, e := range exchanges {
		replies = append(replies, e.reply)
		if len(hzPragmas(e.request, "horizon")) == 0 {
			continue
		}
		scoped++
		if len(hzPragmas(e.request, "horizon-structural")) != 1 || len(hzPragmas(e.request, "horizon-seed")) != 1 {
			return expectf("request %d: horizon-structural %q, horizon-seed %q", i,
				hzPragmas(e.request, "horizon-structural"), hzPragmas(e.request, "horizon-seed"))
		}
		if got := hzPragmas(e.reply, "horizon-served"); len(got) != 1 || strings.Join(got[0], " ") != "structural" {
			return expectf("reply %d advertises %q", i, got)
		}
	}
	if scoped == 0 {
		return expectf("no request of %d carried the horizon pragma", len(exchanges))
	}
	return hzCrossed(replies, x.below())
}

// hzZitSameState compares what two zit repositories derive: each check-in
// of k described, the branches and the leaves.
func hzZitSameState(t *testing.T, bin, home, a, b string, k []string) error {
	t.Helper()
	state := func(dir string) string {
		var out strings.Builder
		for _, u := range k {
			out.WriteString(zitRun(t, bin, home, dir, "info", u))
		}
		out.WriteString(zitRun(t, bin, home, dir, "branch"))
		out.WriteString(zitRun(t, bin, home, dir, "leaves"))
		return out.String()
	}
	if got, want := state(a), state(b); got != want {
		return expectf("derived state differs from dense:\nstructural:\n%s\ndense:\n%s", got, want)
	}
	return nil
}
