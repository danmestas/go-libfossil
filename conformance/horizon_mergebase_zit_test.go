package conformance

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/sync"
)

// TestZitHorizonMergeBaseInterop runs the merge-base query
// (draft-fossil-horizon-00 §6.2.4) between go-libfossil and zit both ways
// over hzStruct. go-libfossil asks `zit serve` for the base of c4 and feat2
// and of feat2 and c1; zit, deepening its structural checkout to c1, which
// lies below its horizon, asks go-libfossil's handler for the base of C and
// c1 and deepens to the answer.
func TestZitHorizonMergeBaseInterop(t *testing.T) {
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

	// go-libfossil asks zit.
	zd := filepath.Join(dir, "zdense")
	zitRun(t, bin, home, dir, "clone", server.URL, zd, "--no-open", "--once")
	peer := &hzRecordingTransport{inner: &sync.HTTPTransport{URL: zitServe(t, bin, home, zd)}}
	r, _, err := sync.Clone(context.Background(), filepath.Join(dir, "dense.fossil"), peer, sync.CloneOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, q := range [][3]string{{x.c4, x.feat2, x.c3}, {x.feat2, x.c1, x.c1}} {
		peer.replies = nil
		res, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true, MergeBaseOf: [2]string{q[0], q[1]}})
		if err != nil || !res.MergeBaseAnswered || res.MergeBase != q[2] {
			t.Fatalf("zit's answer for %.10s %.10s: %v answered=%v base=%q, want %s",
				q[0], q[1], err, res.MergeBaseAnswered, res.MergeBase, q[2])
		}
		if got := hzPragmas(peer.replies[0], "horizon-mergebase"); !slices.EqualFunc(got, [][]string{q[:]}, slices.Equal) {
			t.Fatalf("zit's answer card %q", got)
		}
	}

	// zit asks go-libfossil.
	zs := filepath.Join(dir, "zstructural")
	if out, err := zitCommand(bin, home, dir, "clone", server.URL, zs, "--horizon="+x.c3, "--structural"); err != nil {
		t.Fatalf("zit's structural clone: %v\n%s", err, out)
	}
	origin.take()
	if out, err := zitCommand(bin, home, zs, "update", x.c1); err != nil || !strings.Contains(out, "deepening") {
		t.Fatalf("zit's deepen to c1: %v\n%s", err, out)
	}
	asked := false
	for _, e := range origin.take() {
		if q := hzPragmas(e.request, "horizon-mergebase"); len(q) != 0 {
			asked = true
			if !slices.EqualFunc(q, [][]string{{x.c3, x.c1}}, slices.Equal) {
				t.Fatalf("zit asked %q", q)
			}
			if got := hzPragmas(e.reply, "horizon-mergebase"); !slices.EqualFunc(got, [][]string{{x.c3, x.c1, x.c1}}, slices.Equal) {
				t.Fatalf("go-libfossil answered zit %q", got)
			}
		}
	}
	if !asked {
		t.Fatal("zit's deepen asked go-libfossil no merge-base query")
	}
	if got := zitRun(t, bin, home, zs, "info", x.c1); !strings.Contains(got, x.c1) {
		t.Fatalf("zit's deepened checkout does not hold c1:\n%s", got)
	}
}
