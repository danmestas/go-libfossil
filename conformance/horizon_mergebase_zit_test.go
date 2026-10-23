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
// over hzStruct, in its list form: the primary first, then one or two
// secondaries, the answer repeating them and appending the pivot.
// go-libfossil asks `zit serve` with one secondary and with two, and zit's
// answer must be go-libfossil's origin's; zit, deepening its structural
// checkout to c1, below its horizon, asks go-libfossil's handler for the
// pivot of c1 over C and deepens to the answer, and merging feat2 into a
// checkout of c4, whose ancestry crosses its horizon, asks the pivot of
// feat2 over c4 although its own walk finds one. zit's merge refuses a
// second merge while one is pending, so it never asks two secondaries.
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
	for _, tc := range []struct {
		hashes []string // the primary, then its secondaries
		want   string   // "" for go-libfossil's origin's answer
	}{
		{[]string{x.c4, x.feat2}, x.c3}, {[]string{x.feat2, x.c1}, x.c1},
		{[]string{x.c4, x.feat2, x.c1}, ""}, {[]string{x.feat2, x.c4, x.c2}, ""}, {[]string{x.c1, x.c4, x.feat2}, ""},
	} {
		query := sync.MergeBaseQuery{Primary: tc.hashes[0], Secondaries: tc.hashes[1:]}
		want := tc.want
		if want == "" {
			own, err := sync.Sync(context.Background(), r, x.peer(), sync.SyncOpts{Pull: true, MergeBaseOf: query})
			if err != nil || !own.MergeBaseAnswered || own.MergeBase == "" {
				t.Fatalf("go-libfossil's answer for %.10s over %.10s: %v %+v", tc.hashes[0], tc.hashes[1:], err, own)
			}
			want = own.MergeBase
		}
		peer.replies = nil
		res, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true, MergeBaseOf: query})
		if err != nil || !res.MergeBaseAnswered || res.MergeBase != want {
			t.Fatalf("zit's answer for %.10s over %.10s: %v answered=%v base=%q, want %s",
				tc.hashes[0], tc.hashes[1:], err, res.MergeBaseAnswered, res.MergeBase, want)
		}
		card := append(slices.Clone(tc.hashes), want)
		if got := hzPragmas(peer.replies[0], "horizon-mergebase"); !slices.EqualFunc(got, [][]string{card}, slices.Equal) {
			t.Fatalf("zit's answer card %q, want %q", got, card)
		}
	}
	// zit answers nothing, not -, when it lacks a named check-in.
	peer.replies = nil
	res, err := sync.Sync(context.Background(), r, peer, sync.SyncOpts{Pull: true,
		MergeBaseOf: sync.MergeBaseQuery{Primary: x.c4, Secondaries: []string{x.feat2, strings.Repeat("ab", 20)}}})
	if err != nil || res.MergeBaseAnswered {
		t.Fatalf("zit answered a query naming a check-in it lacks: %v %+v", err, res)
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
			if !slices.EqualFunc(q, [][]string{{x.c1, x.c3}}, slices.Equal) {
				t.Fatalf("zit asked %q, want the pivot of c1 over C", q)
			}
			if got := hzPragmas(e.reply, "horizon-mergebase"); !slices.EqualFunc(got, [][]string{{x.c1, x.c3, x.c1}}, slices.Equal) {
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

	// zit's merge asks whenever its ancestry crosses the horizon (§5.5).
	zm := filepath.Join(dir, "zmerge")
	if out, err := zitCommand(bin, home, dir, "clone", server.URL, zm, "--horizon="+x.c3, "--structural"); err != nil {
		t.Fatalf("zit's structural clone: %v\n%s", err, out)
	}
	zitRun(t, bin, home, zm, "update", x.c4)
	origin.take()
	if out, err := zitCommand(bin, home, zm, "merge", x.feat2); err != nil {
		t.Fatalf("zit's merge of feat2: %v\n%s", err, out)
	}
	asked = false
	for _, e := range origin.take() {
		if q := hzPragmas(e.request, "horizon-mergebase"); len(q) != 0 {
			asked = true
			if !slices.EqualFunc(q, [][]string{{x.feat2, x.c4}}, slices.Equal) {
				t.Fatalf("zit's merge asked %q, want the pivot of feat2 over c4", q)
			}
			if got := hzPragmas(e.reply, "horizon-mergebase"); !slices.EqualFunc(got, [][]string{{x.feat2, x.c4, x.c3}}, slices.Equal) {
				t.Fatalf("go-libfossil answered zit's merge %q", got)
			}
		}
	}
	if !asked {
		t.Fatal("zit's merge across the horizon asked go-libfossil no merge-base query")
	}
}
