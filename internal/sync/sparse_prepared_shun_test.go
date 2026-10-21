package sync

import (
	"context"
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Invalidate the proof after preparation, then verify the actual dense fallback
// emits neither the newly shunned payload nor a dependency on that basis.
func TestPreparedSparseServiceShunFallback(t *testing.T) {
	for _, private := range []bool{false, true} {
		for _, requested := range []bool{false, true} {
			t.Run(fmt.Sprintf("private=%v/gimme=%v", private, requested), func(t *testing.T) {
				r, baseUUID, targetUUID := shunSendFixture(t, private)
				service, err := prepareSparseService(context.Background(), r, HandleOpts{})
				if err != nil {
					t.Fatal(err)
				}
				defer service.Close()
				if _, err := r.DB().Exec("INSERT INTO shun(uuid,mtime) VALUES(?,0)", baseUUID); err != nil {
					t.Fatal(err)
				}
				cards := []xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{"src"}}}
				cards = append(cards, shunSendCards(private, requested, baseUUID, targetUUID)...)
				transport := &preparedSparseTransport{service: service}
				resp, err := transport.Exchange(context.Background(), &xfer.Message{Cards: cards})
				if err != nil {
					t.Fatal(err)
				}
				if transport.applied != 0 {
					t.Fatal("stale proof applied")
				}
				assertShunSend(t, resp, baseUUID, targetUUID)
			})
		}
	}
}
