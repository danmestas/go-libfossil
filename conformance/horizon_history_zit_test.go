package conformance

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestZitHorizonHistoryURLInterop runs zit's horizon clone and pull against
// go-libfossil's handler naming a history URL in horizon-served
// (draft-fossil-horizon-00 §6.2.5): zit takes the level past the optional
// URL and keeps its horizon. zit names no URL of its own.
func TestZitHorizonHistoryURLInterop(t *testing.T) {
	bin := zitHorizonBinary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	g := newHZSyncGraph(t)
	const url = "https://example.org/history"
	if err := g.f.r.SetConfig(repo.SettingHorizonHistoryURL, url); err != nil {
		t.Fatal(err)
	}
	c := hzUUID(t, g.f.r, g.c3)
	origin := &hzRecorder{r: g.f.r}
	server := httptest.NewServer(origin)
	defer server.Close()

	zhorizon := filepath.Join(dir, "zhorizon")
	zitRun(t, bin, home, dir, "clone", "--no-open", "--horizon="+c, server.URL, zhorizon)
	hzCommit(t, g.f, "c5", []libfossil.FslID{g.m}, nil)
	zitRun(t, bin, home, zhorizon, "pull")
	served := 0
	for i, e := range origin.take() {
		for _, values := range hzPragmas(e.reply, "horizon-served") {
			if strings.Join(values, " ") != "content "+url {
				t.Fatalf("reply %d to zit carries horizon-served %q", i, values)
			}
			served++
		}
	}
	if served == 0 {
		t.Fatal("no reply to zit's horizon clone and pull named the history URL")
	}
	if got := zitElidedCount(t, zitRun(t, bin, home, zhorizon, "verify")); got != len(g.below) {
		t.Fatalf("zit's horizon store elided %d, want %d: it did not keep its horizon", got, len(g.below))
	}
}
