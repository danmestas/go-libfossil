package sync

import (
	"context"
	"reflect"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
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

func TestSparseCloneRefusalFatal(t *testing.T) {
	r := setupSyncTestRepo(t)
	cs := &cloneSession{repo: r, seqno: 1}
	err := cs.processResponse(context.Background(), 0, &xfer.Message{Cards: []xfer.Card{&xfer.ErrorCard{Message: "Sparse cone refuses clone"}}})
	if err == nil || !strings.Contains(err.Error(), "Sparse cone refuses clone") {
		t.Fatalf("refused clone returned %v", err)
	}
}

func TestSparseServerFlatClone(t *testing.T) {
	r := setupSyncTestRepo(t)
	kept := storeAndLink(t, r, []byte("keep me"))
	out := storeAndLink(t, r, []byte("elide me"))
	data, err := (&deck.Deck{C: "flat sparse", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "out/a", UUID: out}, {Name: "src/a", UUID: kept}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	spine := storeAndLink(t, r, data)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cards := []xfer.Card{versionPragma("client-version"), sendSubmanifests(), &xfer.CloneCard{Version: 3, SeqNo: 1}}
	cards = append(cards, conePragmas(c)...)
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, card := range findCards[*xfer.CFileCard](resp) {
		seen[card.UUID] = true
	}
	if seen[out] {
		t.Fatal("server sent out-of-cone file")
	}
	if !seen[kept] || !seen[spine] {
		t.Fatalf("server omitted kept artifacts: %v", seen)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	dense, err := HandleSync(context.Background(), r, &xfer.Message{Cards: cards[:3]})
	if err != nil {
		t.Fatal(err)
	}
	if len(dense.Cards) != 1 || len(findCards[*xfer.ErrorCard](dense)) != 1 {
		t.Fatalf("sparse origin did not refuse dense clone: %v", dense.Cards)
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
