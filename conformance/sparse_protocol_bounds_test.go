package conformance

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Depth rejection does not cover the visited-node cap or cold raw-reference
// preprocessing. This case cannot close the per-round traversal requirement.
func scProtocolBoundsCases() []scCase {
	return []scCase{
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

func TestSparseCloneExcludedRunFinalArtifact(t *testing.T) {
	for _, private := range []bool{true, false} {
		t.Run(fmt.Sprintf("private=%v", private), func(t *testing.T) {
			scProtocolCloneExcludedRun(t, private)
		})
	}
}

func scProtocolCloneExcludedRun(t *testing.T, private bool) {
	t.Helper()
	origin := newSCSyncRepo(t, map[string]string{"src/initial": "initial\n"})
	var outside []deck.FileCard
	for i := 0; i < 2051; i++ {
		rid, uuid, err := blob.Store(origin.r.DB(), []byte(fmt.Sprintf("excluded candidate %d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if private {
			if _, err := origin.r.DB().Exec("INSERT INTO private(rid) VALUES(?)", rid); err != nil {
				t.Fatal(err)
			}
		} else {
			outside = append(outside, deck.FileCard{Name: fmt.Sprintf("zzz/%04d", i), UUID: uuid})
		}
	}
	_, wanted, err := blob.Store(origin.r.DB(), []byte("final in-cone content\n"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := (&deck.Deck{C: "late in-cone artifact", D: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), U: deck.User("u"), F: append([]deck.FileCard{{Name: "src/final", UUID: wanted}}, outside...)}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := blob.Store(origin.r.DB(), data); err != nil {
		t.Fatal(err)
	}
	peer := &scSyncPeer{r: origin.r}
	cloned, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"), peer,
		sync.CloneOpts{StoreCone: scCone(t, []string{"src"}, nil), PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cloned.Close()
	if err := scSyncHolding(cloned, "final in-cone content\n", true); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 2050} {
		if err := scSyncHolding(cloned, fmt.Sprintf("excluded candidate %d", i), false); err != nil {
			t.Fatal(err)
		}
	}
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
