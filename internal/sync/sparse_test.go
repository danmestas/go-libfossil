package sync

import (
	"reflect"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestConePragmasEscaping(t *testing.T) {
	c, err := cone.New([]string{"src space", "literal*"}, []string{"src space/private"})
	if err != nil {
		t.Fatal(err)
	}
	cards := conePragmas(c)
	got := make([]string, 0, len(cards))
	for _, card := range cards {
		p := card.(*xfer.PragmaCard)
		got = append(got, p.Name+" "+strings.Join(p.Values, " "))
	}
	want := []string{`cone literal*`, `cone src\sspace`, `cone-not src\sspace/private`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cards = %q, want %q", got, want)
	}
	round, err := requestCone(cards)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round.Serialize(), c.Serialize()) {
		t.Fatal("cone changed across pragmas")
	}
}

func TestConePragmasBounds(t *testing.T) {
	for _, cards := range [][]xfer.Card{
		{&xfer.PragmaCard{Name: "cone", Values: []string{"../escape"}}},
		{&xfer.PragmaCard{Name: "cone", Values: []string{strings.Repeat("a", cone.MaxMemberLen+1)}}},
		{&xfer.PragmaCard{Name: "cone-not", Values: []string{"src"}}},
		{&xfer.PragmaCard{Name: "cone", Values: []string{"src", "extra"}}},
	} {
		if _, err := requestCone(cards); err == nil {
			t.Fatalf("accepted invalid cone %v", cards)
		}
	}
	cards := make([]xfer.Card, cone.MaxMembers+1)
	for i := range cards {
		cards[i] = &xfer.PragmaCard{Name: "cone", Values: []string{"src"}}
	}
	if _, err := requestCone(cards); err == nil {
		t.Fatal("accepted excessive cone members")
	}
	c, err := requestCone([]xfer.Card{&xfer.PragmaCard{Name: "unknown", Values: []string{"anything"}}})
	if err != nil || c != nil {
		t.Fatalf("unknown pragma: cone=%v err=%v", c, err)
	}
}
