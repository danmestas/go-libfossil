package sync

import (
	"context"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func servedValues(t *testing.T, r *repo.Repo) [][]string {
	t.Helper()
	cards := append([]xfer.Card{&xfer.PullCard{ServerCode: "x", ProjectCode: testPC}},
		conePragmas(testHorizonCone(t, cone.DepthContent), noSeed)...)
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, card := range resp.Cards {
		if p, ok := card.(*xfer.PragmaCard); ok && p.Name == horizonServedPragma {
			out = append(out, p.Values)
		}
	}
	return out
}

// draft-fossil-horizon-00 §6.2.5: an origin whose horizon-history-url is
// set appends it to horizon-served; unset, or stored invalid by a path
// that bypassed the setting's check, it emits the level alone.
func TestHorizonServedHistoryURL(t *testing.T) {
	r := setupSyncTestRepo(t)
	if got := servedValues(t, r); len(got) != 1 || strings.Join(got[0], " ") != "content" {
		t.Fatalf("unset: horizon-served %q, want [content]", got)
	}
	const url = "https://example.org/history"
	if err := r.SetConfig(repo.SettingHorizonHistoryURL, url); err != nil {
		t.Fatal(err)
	}
	if got := servedValues(t, r); len(got) != 1 || strings.Join(got[0], " ") != "content "+url {
		t.Fatalf("set: horizon-served %q, want [content %s]", got, url)
	}
	if _, err := r.DB().Exec("UPDATE config SET value=? WHERE name=?",
		"https://example.org/a b", repo.SettingHorizonHistoryURL); err != nil {
		t.Fatal(err)
	}
	if got := servedValues(t, r); len(got) != 1 || strings.Join(got[0], " ") != "content" {
		t.Fatalf("stored invalid: horizon-served %q, want [content]", got)
	}
}

// A client records the URL a horizon-served pragma carries when it is a
// valid one, and ignores an invalid one while still taking the level.
func TestServedHistoryURL(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   string
	}{
		{[]string{"content"}, ""},
		{[]string{"content", "https://example.org/h"}, "https://example.org/h"},
		{[]string{"structural", "http://example.org/h"}, "http://example.org/h"},
		{[]string{"content", "ftp://example.org/h"}, ""},
		{[]string{"content", "https://example.org/h", "extra"}, ""},
	} {
		got := servedHistoryURL(&xfer.PragmaCard{Name: horizonServedPragma, Values: tc.values})
		if got != tc.want {
			t.Errorf("servedHistoryURL(%q) = %q, want %q", tc.values, got, tc.want)
		}
	}
}
