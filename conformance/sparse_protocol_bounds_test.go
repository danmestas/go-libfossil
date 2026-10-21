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

func scProtocolBoundsCases() []scCase {
	return []scCase{
		{Case{ID: "sparsecone.8.depth-wire-bound", Req: "SC-8-2"}, scProtocolDepthBound},
		{Case{ID: "sparsecone.8.visited-wire-bound", Req: "SC-8-2"}, scProtocolVisitedBound},
		{Case{ID: "sparsecone.6.2.round-walk-bound", Req: "SC-6.2-1"}, scProtocolRoundWalk},
		{Case{ID: "sparsecone.6.2.round-work-bound", Req: "SC-6.2-1"}, scProtocolRoundWork},
		{Case{ID: "sparsecone.6.2.excluded-rounds", Req: "SC-6.2-1"}, scProtocolExcludedRounds},
		{Case{ID: "sparsecone.6.2.parent-diff-flat", Req: "SC-6.2-1"}, scProtocolParentDiffFlat},
		{Case{ID: "sparsecone.6.2.parent-diff-tree", Req: "SC-6.2-1"}, scProtocolParentDiffTree},
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

type scHandleWork struct {
	classified []int
}

func (o *scHandleWork) Started(ctx context.Context, _ sync.SessionStart) context.Context { return ctx }
func (o *scHandleWork) RoundStarted(ctx context.Context, _ int) context.Context          { return ctx }
func (o *scHandleWork) RoundCompleted(context.Context, int, sync.RoundStats)             {}
func (o *scHandleWork) Completed(context.Context, sync.SessionEnd, error)                {}
func (o *scHandleWork) Error(context.Context, error)                                     {}
func (o *scHandleWork) HandleStarted(ctx context.Context, _ sync.HandleStart) context.Context {
	return ctx
}
func (o *scHandleWork) HandleCompleted(_ context.Context, end sync.HandleEnd) {
	o.classified = append(o.classified, end.SparseClassified)
}
func (o *scHandleWork) TableSyncStarted(context.Context, sync.TableSyncStart) {}
func (o *scHandleWork) TableSyncCompleted(context.Context, sync.TableSyncEnd) {}

// An excluded run longer than one dense batch precedes many check-ins. The
// first round must classify only what its window holds: work proportional to
// the batch it walks, never to the check-ins the store holds behind it.
func scProtocolRoundWork(t *testing.T) error {
	const checkins = 400
	origin := newSCSyncRepo(t, map[string]string{"src/initial": "initial\n"})
	run, err := scProtocolExcludedRun(origin, "zzz/run", sync.DefaultCloneBatchBytes/(1<<20)+2)
	if err != nil {
		return err
	}
	if err := scProtocolCheckinChain(origin, run, checkins); err != nil {
		return err
	}
	work := &scHandleWork{}
	peer := &scSyncPeer{r: origin.r, opts: sync.HandleOpts{Observer: work}}
	first, err := peer.Exchange(context.Background(), &xfer.Message{Cards: []xfer.Card{
		&xfer.PragmaCard{Name: "cone", Values: []string{"src"}},
		&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true},
	}})
	if err != nil {
		return err
	}
	cursor := scProtocolCursor(first)
	if cursor <= 0 || cursor > len(run)+8 {
		return fmt.Errorf("first round cursor = %d, want inside the excluded run of %d", cursor, len(run))
	}
	if got := work.classified[0]; got == 0 || got > cursor+8 {
		return fmt.Errorf("first round classified %d artifacts below cursor %d over %d check-ins",
			got, cursor, checkins)
	}
	cloned, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"), peer,
		sync.CloneOpts{StoreCone: scCone(t, []string{"src"}, nil), PullPhantoms: true})
	if err != nil {
		return err
	}
	defer cloned.Close()
	for text, want := range map[string]bool{"shared in cone\n": true, "checkin 0\n": false, "checkin 399\n": false} {
		if err := scSyncHolding(cloned, text, want); err != nil {
			return err
		}
	}
	return nil
}

// scProtocolExcludedRun stores incompressible megabyte blobs and returns the
// F cards naming them outside the cone.
func scProtocolExcludedRun(origin *scRepo, dir string, pieces int) ([]deck.FileCard, error) {
	const piece = 1 << 20
	noise := rand.New(rand.NewPCG(6, 2))
	var outside []deck.FileCard
	for i := 0; i < pieces; i++ {
		data := make([]byte, piece)
		for j := range data {
			data[j] = byte(noise.Uint32())
		}
		_, uuid, err := blob.Store(origin.r.DB(), data)
		if err != nil {
			return nil, err
		}
		outside = append(outside, deck.FileCard{Name: fmt.Sprintf("%s/%04d", dir, i), UUID: uuid})
	}
	return outside, nil
}

// scProtocolCheckinChain stores a linear history: every check-in names one
// shared in-cone file and its own outside file; the first also names run.
func scProtocolCheckinChain(origin *scRepo, run []deck.FileCard, n int) error {
	_, shared, err := blob.Store(origin.r.DB(), []byte("shared in cone\n"))
	if err != nil {
		return err
	}
	when := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	parent := ""
	for i := 0; i < n; i++ {
		_, own, err := blob.Store(origin.r.DB(), []byte(fmt.Sprintf("checkin %d\n", i)))
		if err != nil {
			return err
		}
		files := []deck.FileCard{{Name: "src/shared", UUID: shared}, {Name: "zzz/own", UUID: own}}
		if i == 0 {
			files = append(files, run...)
		}
		d := &deck.Deck{C: fmt.Sprintf("chain %d", i), D: when.Add(time.Duration(i) * time.Second),
			U: deck.User("u"), F: files}
		if parent != "" {
			d.P = []string{parent}
		}
		data, err := d.Marshal()
		if err != nil {
			return err
		}
		if _, parent, err = blob.Store(origin.r.DB(), data); err != nil {
			return err
		}
	}
	return nil
}

func scProtocolCursor(m *xfer.Message) int {
	for _, card := range m.Cards {
		if c, ok := card.(*xfer.CloneSeqNoCard); ok {
			return c.SeqNo
		}
	}
	return -1
}

// An excluded run spanning more than two dense batches leaves a whole round
// with nothing in the cone to send; its advancing cursor still carries the
// clone on to the in-cone artifact behind the run.
func scProtocolExcludedRounds(t *testing.T) error {
	origin := newSCSyncRepo(t, map[string]string{"src/initial": "initial\n"})
	run, err := scProtocolExcludedRun(origin, "zzz/run", 2*sync.DefaultCloneBatchBytes/(1<<20)+2)
	if err != nil {
		return err
	}
	_, wanted, err := blob.Store(origin.r.DB(), []byte("after the run\n"))
	if err != nil {
		return err
	}
	data, err := (&deck.Deck{C: "after the run", D: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
		U: deck.User("u"), F: append([]deck.FileCard{{Name: "src/after", UUID: wanted}}, run...)}).Marshal()
	if err != nil {
		return err
	}
	if _, _, err := blob.Store(origin.r.DB(), data); err != nil {
		return err
	}
	peer := &scSyncPeer{r: origin.r}
	cloned, _, err := sync.Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"), peer,
		sync.CloneOpts{StoreCone: scCone(t, []string{"src"}, nil)})
	if err != nil {
		return err
	}
	defer cloned.Close()
	empty := 0
	for _, resp := range peer.replies {
		if len(scSyncPayloads(resp)) == 0 && scProtocolCursor(resp) > 0 {
			empty++
		}
	}
	if empty == 0 {
		return fmt.Errorf("no round fell wholly inside the excluded run")
	}
	return scSyncHolding(cloned, "after the run\n", true)
}
