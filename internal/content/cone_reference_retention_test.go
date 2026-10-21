package content

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// Keep classification facts, not full wiki/forum payloads or cluster inventories.
// Increasing those irrelevant fields must not increase retained deck storage.
func TestConeReferenceFactsDiscardIrrelevantPayloads(t *testing.T) {
	d := setupTestDB(t)
	var artifacts []*deck.Deck
	for _, size := range []int{0, 1 << 20} {
		body := bytes.Repeat([]byte("w"), size)
		artifacts = append(artifacts,
			&deck.Deck{L: "wiki", D: time.Now().UTC(), U: deck.User("u"), W: body},
			&deck.Deck{H: "forum", D: time.Now().UTC(), U: deck.User("u"), W: body})
	}
	var members []string
	for i := 0; i < 2000; i++ {
		members = append(members, hash.SHA1([]byte(fmt.Sprint(i))))
	}
	sort.Strings(members)
	artifacts = append(artifacts, &deck.Deck{M: members})
	for _, item := range artifacts {
		data, err := item.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := blob.Store(d, data); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := LoadConeReferenceFacts(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.records) != len(artifacts) {
		t.Fatalf("parsed records=%d want%d", len(facts.records), len(artifacts))
	}
	for _, record := range facts.records {
		d := record.deck
		if len(d.W) != 0 || len(d.M) != 0 || d.L != "" || d.H != "" {
			t.Fatalf("retained irrelevant payload: W=%d M=%d L=%q H=%q", len(d.W), len(d.M), d.L, d.H)
		}
	}
}

// Ordinary discovery also rejects an expansion reservation above its bound
// before inflation/parser work; it must not reserve memory from a lying size.
func TestConeReferenceFactsOrdinaryPreflightBound(t *testing.T) {
	d := setupTestDB(t)
	rid, _, err := blob.Store(d, []byte("small stored bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("UPDATE blob SET size=? WHERE rid=?", int64(1<<30), rid); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConeReferenceFacts(context.Background(), d); err == nil {
		t.Fatal("ordinary loader ignored expansion budget")
	}
}
