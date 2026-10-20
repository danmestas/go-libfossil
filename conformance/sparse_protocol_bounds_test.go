package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Candidate pagination does not cover cold raw-reference preprocessing or pull
// inventories. These cases cannot close the per-round traversal requirement.
func scProtocolBoundsCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.6.2.excluded-candidate-budget", Req: "SC-6.2-1"}, scProtocolCandidateBudget},
		{Case{ID: "sparsecone.8.depth-wire-bound", Req: "SC-8-2"}, scProtocolDepthBound},
	}
}

func TestSparseProtocolBounds(t *testing.T) {
	for _, c := range scProtocolBoundsCases() {
		t.Run(c.ID, func(t *testing.T) {
			if err := c.run(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func scProtocolCandidateBudget(t *testing.T) error {
	origin := newSCSyncRepo(t, map[string]string{"src/kept": "covered\n"})
	files := make([]deck.FileCard, 2051)
	outside := map[string]bool{}
	for i := range files {
		_, uuid, err := blob.Store(origin.r.DB(), []byte(fmt.Sprintf("outside candidate %d", i)))
		if err != nil {
			return err
		}
		files[i] = deck.FileCard{Name: fmt.Sprintf("zzz/%04d", i), UUID: uuid}
		outside[uuid] = true
	}
	data, err := (&deck.Deck{C: "outside inventory", D: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), U: deck.User("u"), F: files}).Marshal()
	if err != nil {
		return err
	}
	if _, _, err := blob.Store(origin.r.DB(), data); err != nil {
		return err
	}
	peer := &scSyncPeer{r: origin.r}
	cursor := 1
	cookie := ""
	for round := 0; round < 4; round++ {
		cards := []xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{"src"}}}
		if cookie != "" {
			cards = append(cards, &xfer.CookieCard{Value: cookie})
		}
		cards = append(cards, &xfer.CloneCard{Version: 3, SeqNo: cursor, SeqNoIsDecimal: true})
		resp, err := peer.Exchange(context.Background(), &xfer.Message{Cards: cards})
		if err != nil {
			return err
		}
		next := -1
		for _, card := range resp.Cards {
			switch c := card.(type) {
			case *xfer.CookieCard:
				cookie = c.Value
			case *xfer.CloneSeqNoCard:
				next = c.SeqNo
			case *xfer.FileCard:
				if outside[c.UUID] {
					return fmt.Errorf("outside file emitted: %s", c.UUID)
				}
			case *xfer.CFileCard:
				if outside[c.UUID] {
					return fmt.Errorf("outside cfile emitted: %s", c.UUID)
				}
			}
		}
		if next == 0 {
			if round < 2 {
				return fmt.Errorf("excluded run exhausted in %d rounds, want at least 3", round+1)
			}
			return nil
		}
		if next <= cursor || next > cursor+1024 {
			return fmt.Errorf("cursor %d -> %d exceeded candidate budget or stalled", cursor, next)
		}
		cursor = next
	}
	return fmt.Errorf("excluded run failed to terminate")
}

func scProtocolDepthBound(t *testing.T) error {
	for _, depth := range []int{16, 17} {
		origin := newSCSyncRepo(t, map[string]string{"src/kept": "covered\n"})
		_, file, err := blob.Store(origin.r.DB(), []byte("tree content"))
		if err != nil {
			return err
		}
		node := &deck.Deck{F: []deck.FileCard{{Name: "src/a", UUID: file}}}
		var uuid string
		for level := 0; level < depth; level++ {
			data, err := node.Marshal()
			if err != nil {
				return err
			}
			_, uuid, err = blob.Store(origin.r.DB(), data)
			if err != nil {
				return err
			}
			node = &deck.Deck{S: []deck.SubmanifestCard{{Name: "src/a", UUID: uuid}}}
		}
		root := &deck.Deck{C: "deep tree", D: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), U: deck.User("u"), S: []deck.SubmanifestCard{{Name: "src/a", UUID: uuid}}}
		data, err := root.Marshal()
		if err != nil {
			return err
		}
		if _, _, err := blob.Store(origin.r.DB(), data); err != nil {
			return err
		}
		peer := &scSyncPeer{r: origin.r}
		_, err = peer.Exchange(context.Background(), &xfer.Message{Cards: []xfer.Card{
			&xfer.PragmaCard{Name: "cone", Values: []string{"src"}},
			&xfer.PragmaCard{Name: "send-submanifests"},
			&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true},
		}})
		if depth == 16 && err != nil {
			return fmt.Errorf("depth 16 refused: %w", err)
		}
		if depth == 17 && (err == nil || !strings.Contains(err.Error(), "tree traversal bound exceeded")) {
			return fmt.Errorf("depth 17 error = %v, want tree traversal bound exceeded", err)
		}
	}
	return nil
}
