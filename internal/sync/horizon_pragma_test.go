package sync

import (
	"bytes"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

const testHorizonHash = "0123456789abcdef0123456789abcdef01234567"

func testHorizonCone(t *testing.T, depth cone.HorizonDepth, include ...string) *cone.Cone {
	t.Helper()
	h, err := cone.NewHorizon(testHorizonHash, cone.RuleLeaves, depth)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cone.NewWithHorizon(include, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pragmaLines(t *testing.T, cards []xfer.Card) []string {
	t.Helper()
	var lines []string
	for _, card := range cards {
		var buf bytes.Buffer
		if err := xfer.EncodeCard(&buf, card); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.TrimSuffix(buf.String(), "\n"))
	}
	return lines
}

// draft-fossil-horizon-00 §6.1: one horizon pragma after the path-cone
// pragmas, spelled as zit spells it, and never horizon-structural: this
// client asks for the content horizon whatever depth its store records.
func TestHorizonPragmaWire(t *testing.T) {
	for _, tc := range []struct {
		depth   cone.HorizonDepth
		include []string
		want    []string
	}{
		{cone.DepthContent, nil, []string{"pragma horizon " + testHorizonHash + " leaves"}},
		{cone.DepthStructural, nil, []string{"pragma horizon " + testHorizonHash + " leaves"}},
		{cone.DepthContent, []string{"src"}, []string{"pragma cone src", "pragma horizon " + testHorizonHash + " leaves"}},
	} {
		c := testHorizonCone(t, tc.depth, tc.include...)
		cards := conePragmas(c)
		if got := pragmaLines(t, cards); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Fatalf("pragmas = %q, want %q", got, tc.want)
		}
		round, err := requestCone(cards)
		if err != nil {
			t.Fatal(err)
		}
		h, ok := round.Horizon()
		if !ok || h.Hash() != testHorizonHash || h.Rule() != cone.RuleLeaves || h.Depth() != cone.DepthContent {
			t.Fatalf("request horizon = %v, %v", h, ok)
		}
		if strings.Join(round.Include(), ",") != strings.Join(tc.include, ",") {
			t.Fatalf("request paths = %q, want %q", round.Include(), tc.include)
		}
	}
}

// A malformed horizon pragma is ignored, as zit ignores one: the request is
// then served as if it carried none (§6.3 from the server's side).
func TestHorizonPragmaMalformedIgnored(t *testing.T) {
	for _, values := range [][]string{
		{testHorizonHash},
		{testHorizonHash, "leaves", "extra"},
		{"0123", "leaves"},
		{testHorizonHash, "ancestors"},
	} {
		c, err := requestCone([]xfer.Card{&xfer.PragmaCard{Name: "horizon", Values: values}})
		if err != nil || c != nil {
			t.Fatalf("horizon %q: cone=%v err=%v, want ignored", values, c, err)
		}
	}
}
