package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestZitHorizonOriginClustersInterop runs a horizon clone, pull and sync
// of go-libfossil against `zit serve` over a store holding clusters, minted
// before zit's copy was cloned and after (draft-fossil-horizon-00 §4.1,
// §4.5, §6.2), and records whether zit's replies name any cluster. However
// zit answers, the horizon store ends holding no cluster.
func TestZitHorizonOriginClustersInterop(t *testing.T) {
	bin := zitHorizonBinary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	g := newHZSyncGraph(t)
	hzClustered(t, g.f.r, "old", content.ClusterThreshold)
	server := httptest.NewServer(&hzRecorder{r: g.f.r})
	defer server.Close()
	zd := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", "--no-open", server.URL, zd)
	zitURL := zitServe(t, bin, home, zd)

	peer := &hzRecordingTransport{inner: &sync.HTTPTransport{URL: zitURL}}
	sent := &hzSentTransport{inner: peer}
	r, _, err := sync.Clone(context.Background(), filepath.Join(dir, "horizon.fossil"), sent,
		sync.CloneOpts{StoreCone: g.c, PullPhantoms: true, Origin: zitURL})
	if err != nil {
		t.Fatalf("go-libfossil's horizon clone from zit: %v", err)
	}
	defer r.Close()
	parent := hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	hzUnclustered(t, g.f.r, "new", content.ClusterThreshold)
	hzCommit(t, g.f, "c6", []libfossil.FslID{parent}, nil)
	zitRun(t, bin, home, zd, "pull", server.URL)
	for _, opts := range []sync.SyncOpts{{Pull: true}, {Pull: true, Push: true}, {Pull: true}} {
		opts.Origin = zitURL
		if _, err := sync.Sync(context.Background(), r, sent, opts); err != nil {
			t.Fatalf("go-libfossil's sync with zit: %v", err)
		}
	}

	dense, _, err := sync.Clone(context.Background(), filepath.Join(dir, "dense.fossil"),
		&sync.HTTPTransport{URL: zitURL}, sync.CloneOpts{})
	if err != nil {
		t.Fatalf("go-libfossil's dense clone from zit: %v", err)
	}
	defer dense.Close()
	clusters, err := hzClusterNames(dense)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) == 0 {
		t.Fatal("fixture: zit's store holds no cluster")
	}
	if err := hzCrossed(peer.replies, clusters); err != nil {
		t.Fatalf("zit's replies to go-libfossil's horizon clone, pull and sync: %v", err)
	}
	if err := hzNoGimme(sent.requests, clusters); err != nil {
		t.Fatalf("go-libfossil's horizon store asked zit: %v", err)
	}
	if hzReqClusters(sent.requests) {
		t.Fatal("go-libfossil's horizon store asked zit for the cluster catalog")
	}
	if err := hzHeld(r, clusters, false); err != nil {
		t.Fatalf("go-libfossil's horizon store from zit: %v", err)
	}

	// Named outright, a cluster is withheld from a horizon gimme. zit still
	// answers req-clusters under a horizon pragma with the catalog, which
	// go-libfossil's handler does not; a store served at its horizon never
	// sends one (horizon.4.5.served-no-cluster-chase), so it is logged.
	code, err := g.f.r.Config("project-code")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := g.c.Horizon()
	horizon := &xfer.PragmaCard{Name: "horizon", Values: []string{h.Hash(), h.Rule().String()}}
	gimmes := []xfer.Card{horizon}
	for _, uuid := range scSorted(clusters) {
		gimmes = append(gimmes, &xfer.GimmeCard{UUID: uuid})
	}
	ask := func(extra ...xfer.Card) error {
		cards := append([]xfer.Card{&xfer.PragmaCard{Name: "client-version", Values: []string{"22800"}},
			&xfer.PullCard{ServerCode: "x", ProjectCode: code}}, extra...)
		resp, err := (&sync.HTTPTransport{URL: zitURL}).Exchange(context.Background(), &xfer.Message{Cards: cards})
		if err != nil {
			t.Fatal(err)
		}
		return hzCrossed([]*xfer.Message{resp}, clusters)
	}
	if ask(gimmes[1:]...) == nil {
		t.Fatal("fixture: zit sent no cluster to a dense gimme naming it")
	}
	if err := ask(gimmes...); err != nil {
		t.Fatalf("zit's reply to a horizon gimme naming its clusters: %v", err)
	}
	if err := ask(horizon, &xfer.PragmaCard{Name: "req-clusters"}); err != nil {
		t.Logf("zit's reply to req-clusters under a horizon: %v", err)
	}
}
