package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/sync"
)

// TestZitHorizonStructuralFallbackInterop runs the structural store's
// fallback (draft-fossil-horizon-00 §6.2.5, §6.3) between go-libfossil and
// zit both ways over hzStruct. A store already at the structural horizon
// whose pull is answered without horizon-served structural falls back to
// the content horizon at the same C and rule, discards its seed, fetches the
// manifests below C and says so. zit's store pulls from go-libfossil's
// handler serving as a horizon-ignorant peer; go-libfossil's store, at a C
// committed after `zit serve`'s repository was cloned, pulls from zit, which
// cannot place C and so cannot seed it.
func TestZitHorizonStructuralFallbackInterop(t *testing.T) {
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

	// zit falls back pulling from go-libfossil.
	zs := filepath.Join(dir, "zstructural")
	if out, err := zitCommand(bin, home, dir, "clone", server.URL, zs, "--no-open", "--once",
		"--horizon="+x.c3, "--structural"); err != nil || strings.Contains(out, "content horizon instead") {
		t.Fatalf("zit's structural clone from go-libfossil: %v\n%s", err, out)
	}
	sidecar := filepath.Join(zs, ".zit-cache", "horizon-seed")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("zit's structural clone holds no seed: %v", err)
	}
	origin.setIgnorant(true)
	out, err := zitCommand(bin, home, zs, "pull", server.URL)
	origin.setIgnorant(false)
	if err != nil || !strings.Contains(out, "content horizon instead") {
		t.Fatalf("zit's pull from a horizon-ignorant go-libfossil: %v\n%s", err, out)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("zit kept its seed through the fallback (%v)", err)
	}
	for _, below := range []string{x.c2, x.c1, x.feat1} {
		if got := zitRun(t, bin, home, zs, "info", below); !strings.Contains(got, below) {
			t.Fatalf("zit's fallen-back store does not hold the manifest %.10s below C:\n%s", below, got)
		}
	}

	// go-libfossil falls back pulling from zit.
	zd := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", server.URL, zd, "--no-open", "--once")
	c5 := hzUUID(t, x.s.f.r, hzCommit(t, x.s.f, "c5", []libfossil.FslID{x.s.c4}, nil))
	r, cloned, err := sync.Clone(context.Background(), filepath.Join(dir, "structural.fossil"),
		&scSyncPeer{r: x.s.f.r}, sync.CloneOpts{
			StoreCone: hzStoreCone(t, c5, cone.RuleDescendants, cone.DepthStructural), PullPhantoms: true,
		})
	if err != nil || cloned.HorizonFallback || cloned.StructuralServed == 0 {
		t.Fatalf("go-libfossil's structural clone at c5: %v %+v", err, cloned)
	}
	defer r.Close()
	if held, err := seed.Held(r.DB()); err != nil || held == nil {
		t.Fatalf("go-libfossil's structural clone holds no seed (%v)", err)
	}
	peer := &hzRecordingTransport{inner: &sync.HTTPTransport{URL: zitServe(t, bin, home, zd)}}
	pulled, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true})
	if err != nil || !pulled.HorizonFallback || pulled.StructuralServed != 0 {
		t.Fatalf("go-libfossil's pull from zit, which cannot place c5: %v %+v", err, pulled)
	}
	if d, err := hzStructDepth(r); err != nil || d != cone.DepthContent {
		t.Fatalf("go-libfossil's stored depth %v (%v), want content", d, err)
	}
	if held, _ := seed.Held(r.DB()); held != nil {
		t.Fatal("go-libfossil kept its seed through the fallback")
	}
	if err := hzHeld(r, map[string]bool{x.c1: true, x.c2: true, x.c3: true, x.c4: true, x.feat1: true}, true); err != nil {
		t.Fatalf("go-libfossil's fallen-back store: %v", err)
	}
}
