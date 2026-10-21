package content

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
)

func TestConeReferenceFactsClassifyWithoutConeKey(t *testing.T) {
	d := setupTestDB(t)
	inside, inUUID, err := blob.Store(d, []byte("inside"))
	if err != nil {
		t.Fatal(err)
	}
	outside, outUUID, err := blob.Store(d, []byte("outside"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := (&deck.Deck{C: "facts", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "src/a", UUID: inUUID}, {Name: "zzz/a", UUID: outUUID}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := blob.Store(d, data); err != nil {
		t.Fatal(err)
	}
	facts, err := LoadConeReferenceFacts(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	left, err := facts.Classify(context.Background(), mustCone(t, []string{"src"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	right, err := facts.Classify(context.Background(), mustCone(t, []string{"zzz"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if left.Outside(inside) || !left.Outside(outside) || !right.Outside(inside) || right.Outside(outside) {
		t.Fatal("request-local classifications share mutable policy state")
	}
	if _, err := d.Exec("UPDATE blob SET content=NULL,size=-1 WHERE rid=?", inside); err != nil {
		t.Fatal(err)
	}
	again, err := facts.Classify(context.Background(), mustCone(t, []string{"src"}, nil))
	if err != nil || again.Outside(inside) || !again.Outside(outside) {
		t.Fatalf("immutable facts changed after database mutation: %v", err)
	}
}
