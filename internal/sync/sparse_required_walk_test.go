package sync

import (
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// A tree whose S cards name one sub-manifest many times must parse that node
// once. The visit limit bounds how many references are followed and the byte
// budget bounds what is held, but neither bounds parsing work: re-parsing a
// large node per reference made the closure walk linear in the reference
// count, long enough to hang a caller with no deadline.
func TestWalkStructuralParsesSharedNodeOnce(t *testing.T) {
	const refs = 64
	shared := strings.Repeat("a", 64)
	root := &deck.Deck{Type: deck.Checkin}
	for i := 0; i < refs; i++ {
		root.S = append(root.S, deck.SubmanifestCard{Name: fmt.Sprintf("dir%03d", i), UUID: shared})
	}
	decks := map[string]*deck.Deck{
		"root": root,
		shared: {Type: deck.Submanifest, F: []deck.FileCard{{Name: "f", UUID: strings.Repeat("b", 64)}}},
	}
	parses := make(map[string]int)
	structural, err := walkStructural([]string{"root"}, func(frame requiredFrame) (*deck.Deck, error) {
		parses[frame.uuid]++
		return decks[frame.uuid], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !structural["root"] || !structural[shared] || len(structural) != 2 {
		t.Fatalf("structural set %v, want root and the shared node", structural)
	}
	if parses[shared] != 1 {
		t.Fatalf("shared node parsed %d times over %d references, want 1", parses[shared], refs)
	}
	if parses["root"] != 1 {
		t.Fatalf("root parsed %d times, want 1", parses["root"])
	}
}

// The same artifact reached as a baseline and as a check-in is two frames:
// each pairing is parsed once, and a missing node ends its branch quietly.
func TestWalkStructuralBaselineIsItsOwnFrame(t *testing.T) {
	base := strings.Repeat("c", 64)
	decks := map[string]*deck.Deck{
		"root": {Type: deck.Checkin, B: base},
		base:   {Type: deck.Checkin, S: []deck.SubmanifestCard{{Name: "gone", UUID: strings.Repeat("d", 64)}}},
	}
	parses := make(map[requiredFrame]int)
	structural, err := walkStructural([]string{"root", base}, func(frame requiredFrame) (*deck.Deck, error) {
		parses[requiredFrame{uuid: frame.uuid, baseline: frame.baseline}]++
		return decks[frame.uuid], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !structural["root"] || !structural[base] {
		t.Fatalf("structural set %v", structural)
	}
	if parses[requiredFrame{uuid: base, baseline: true}] != 1 ||
		parses[requiredFrame{uuid: base, baseline: false}] != 1 {
		t.Fatalf("baseline frames parsed %v, want once each", parses)
	}
}
