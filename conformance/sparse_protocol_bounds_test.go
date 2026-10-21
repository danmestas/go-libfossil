package conformance

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The round-walk case bounds the clone candidate walk; it does not bound the
// whole-store keep-set discovery that precedes it on the ordinary path.
func scProtocolBoundsCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.8.depth-wire-bound", Req: "SC-8-2"}, scProtocolDepthBound},
		{Case{ID: "sparsecone.8.visited-wire-bound", Req: "SC-8-2"}, scProtocolVisitedBound},
		{Case{ID: "sparsecone.6.2.round-walk-bound", Req: "SC-6.2-1"}, scProtocolRoundWalk},
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

// One interior node of 1024 children shared by many check-ins: each root costs
// 1025 visits, so 1023 roots stay under the 1<<20 visited-node bound and 1024
// roots cross it. Depth stays at two, so only the visited bound can refuse.
func scProtocolVisitedBound(t *testing.T) error {
	for _, roots := range []int{1023, 1024} {
		origin := newSCSyncRepo(t, map[string]string{"src/kept": "covered\n"})
		children := make([]deck.SubmanifestCard, 0, 1024)
		for i := 0; i < 1024; i++ {
			name := "src/a"
			if i > 0 {
				name = fmt.Sprintf("src/a%04d", i)
			}
			children = append(children, deck.SubmanifestCard{Name: name, UUID: hash.SHA1([]byte(name))})
		}
		data, err := (&deck.Deck{S: children}).Marshal()
		if err != nil {
			return err
		}
		_, node, err := blob.Store(origin.r.DB(), data)
		if err != nil {
			return err
		}
		if err := scProtocolSharedRoots(origin, node, roots); err != nil {
			return err
		}
		peer := &scSyncPeer{r: origin.r}
		_, err = peer.Exchange(context.Background(), &xfer.Message{Cards: []xfer.Card{
			&xfer.PragmaCard{Name: "cone", Values: []string{"src"}},
			&xfer.PragmaCard{Name: "send-submanifests"},
			&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true},
		}})
		if roots == 1023 && err != nil {
			return fmt.Errorf("%d shared roots refused: %w", roots, err)
		}
		if roots == 1024 && (err == nil || !strings.Contains(err.Error(), "tree traversal bound exceeded")) {
			return fmt.Errorf("%d shared roots error = %v, want tree traversal bound exceeded", roots, err)
		}
	}
	return nil
}

func scProtocolSharedRoots(origin *scRepo, node string, roots int) error {
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < roots; i++ {
		root := &deck.Deck{C: fmt.Sprintf("shared tree %d", i), D: when.Add(time.Duration(i) * time.Second),
			U: deck.User("u"), S: []deck.SubmanifestCard{{Name: "src/a", UUID: node}}}
		data, err := root.Marshal()
		if err != nil {
			return err
		}
		if _, _, err := blob.Store(origin.r.DB(), data); err != nil {
			return err
		}
	}
	return nil
}

// The excluded run outweighs one dense clone batch, so a bounded first round
// stops inside it, sends nothing in cone, and names a cursor that later rounds
// resume from until the clone converges on the late in-cone artifact.
func scProtocolRoundWalk(t *testing.T) error {
	origin := newSCSyncRepo(t, map[string]string{"src/initial": "initial\n"})
	const piece = 1 << 20
	noise := rand.New(rand.NewPCG(6, 2))
	var outside []deck.FileCard
	for i := 0; i < sync.DefaultCloneBatchBytes/piece+2; i++ {
		data := make([]byte, piece)
		for j := range data {
			data[j] = byte(noise.Uint32())
		}
		_, uuid, err := blob.Store(origin.r.DB(), data)
		if err != nil {
			return err
		}
		outside = append(outside, deck.FileCard{Name: fmt.Sprintf("zzz/%04d", i), UUID: uuid})
	}
	finalRID, wanted, err := blob.Store(origin.r.DB(), []byte("final in-cone content\n"))
	if err != nil {
		return err
	}
	data, err := (&deck.Deck{C: "late in-cone artifact", D: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), U: deck.User("u"), F: append([]deck.FileCard{{Name: "src/final", UUID: wanted}}, outside...)}).Marshal()
	if err != nil {
		return err
	}
	if _, _, err := blob.Store(origin.r.DB(), data); err != nil {
		return err
	}
	peer := &scSyncPeer{r: origin.r}
	first, err := peer.Exchange(context.Background(), &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "cone", Values: []string{"src"}},
		&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true},
	}})
	if err != nil {
		return err
	}
	if err := scProtocolBoundedRound(first, wanted, int(finalRID)); err != nil {
		return err
	}
	cloned, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"), peer,
		sync.CloneOpts{StoreCone: scCone(t, []string{"src"}, nil), PullPhantoms: true})
	if err != nil {
		return err
	}
	defer cloned.Close()
	if err := scSyncHolding(cloned, "final in-cone content\n", true); err != nil {
		return err
	}
	if _, held, err := content.CheckAvailableByUUID(cloned.DB(), outside[0].UUID); err != nil || held {
		return fmt.Errorf("excluded artifact held=%v error=%v", held, err)
	}
	return nil
}

func scProtocolBoundedRound(m *xfer.Message, wanted string, finalRID int) error {
	if _, sent := scSyncPayloads(m)[wanted]; sent {
		return fmt.Errorf("first round walked past one dense batch to the late in-cone artifact")
	}
	for _, card := range m.Cards {
		if c, ok := card.(*xfer.CloneSeqNoCard); ok {
			if c.SeqNo <= 0 || c.SeqNo > finalRID {
				return fmt.Errorf("first round cursor = %d, want inside the excluded run", c.SeqNo)
			}
			return nil
		}
	}
	return fmt.Errorf("first round carried no clone_seqno")
}
