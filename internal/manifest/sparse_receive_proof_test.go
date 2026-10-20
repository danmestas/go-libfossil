package manifest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
)

// Reusing one receive linker across transactions must not preserve an Outside
// proof after a newly received artifact makes the omitted node structural.
func TestSparseReceiveProofRefreshesAfterRawAliasArrival(t *testing.T) {
	f, rid, outside, _ := partialMetadataFixture(t)
	linker, err := NewReceiveLinker(f.r)
	if err != nil {
		t.Fatal(err)
	}
	data, err := content.Expand(f.r.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	root, err := deck.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	receiveSparseProofDeck(t, f, linker, root)
	if _, err := linker.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSparseRows(t, f, int64(rid), 1, 0, 1)
	if linker.state.guard.coneReferences != nil {
		t.Fatal("raw classification proof escaped its batch transaction")
	}
	receiveSparseProofDeck(t, f, linker, &deck.Deck{
		D: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), U: deck.User("u"),
		T: []deck.TagCard{{Type: deck.TagSingleton, Name: "sym-required", UUID: outside}},
	})
	root.C = "later check-in with newly required gap"
	root.D = root.D.Add(time.Hour)
	child := receiveSparseProofDeck(t, f, linker, root)
	if _, err := linker.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", child).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("newly required gap published %d metadata rows using stale proof", n)
	}
}

func TestSparseReceiveProofRecognizesNewOutsideNode(t *testing.T) {
	f, rid, _, _ := partialMetadataFixture(t)
	linker, err := NewReceiveLinker(f.r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := linker.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := content.Expand(f.r.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	root, err := deck.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	root.C = "new outside node after first finalize"
	root.S[1].UUID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	child := receiveSparseProofDeck(t, f, linker, root)
	if _, err := linker.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSparseRows(t, f, child, 1, 0, 1)
	if linker.state.guard.coneReferences != nil {
		t.Fatal("finalize retained transaction-scoped raw proof")
	}
}

func TestSparseProofReusedWithinUnchangedTransaction(t *testing.T) {
	f, rid, _, _ := partialMetadataFixture(t)
	data, err := content.Expand(f.r.DB(), rid)
	if err != nil {
		t.Fatal(err)
	}
	d, err := deck.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	st := newLinkState(receiveCacheBytes)
	if err := f.r.WithTx(func(tx *db.Tx) error {
		ready, err := crosslinkPartialCheckin(context.Background(), tx, rid, d, st.guard, st.leaves)
		if err != nil {
			return err
		}
		if !ready || st.guard.coneReferences == nil {
			t.Fatal("partial check-in did not load transaction proof")
		}
		proof := st.guard.coneReferences
		ready, err = crosslinkPartialCheckin(context.Background(), tx, rid, d, st.guard, st.leaves)
		if !ready || st.guard.coneReferences != proof {
			t.Error("unchanged transaction did not reuse its proof")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSparseReceiveProofClearedAfterBatchRollback(t *testing.T) {
	f, rid, _, _ := partialMetadataFixture(t)
	for i := 0; i < 768; i++ {
		f.storeBytes([]byte(fmt.Sprintf("ordinary receive payload %d", i)))
	}
	linker, err := NewReceiveLinker(f.r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := linker.Finalize(newPollCancelCtx(2)); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted finalize = %v, want cancellation", err)
	}
	if linker.state.guard.coneReferences != nil {
		t.Fatal("rolled-back batch retained its raw proof")
	}
	var n int
	if err := f.r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", rid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("interrupted finalize committed %d partial events", n)
	}
	if _, err := linker.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSparseRows(t, f, int64(rid), 1, 0, 1)
}

func receiveSparseProofDeck(t *testing.T, f *treeFixture, linker *ReceiveLinker, d *deck.Deck) int64 {
	t.Helper()
	data, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var result int64
	if err := linker.WithTx(f.r, func(tx *db.Tx) error {
		rid, uuid, err := blob.Store(tx, data)
		if err != nil {
			return err
		}
		result = int64(rid)
		return linker.LinkStored(context.Background(), tx, rid, uuid, data)
	}); err != nil {
		t.Fatal(err)
	}
	return result
}
