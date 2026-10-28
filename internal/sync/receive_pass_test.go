package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// lateBasisFixture is a basis, and a delta against it that gives other
// content than the name it is sent under.
type lateBasisFixture struct {
	base, baseUUID, claimed string
	evil                    []byte
}

func newLateBasisFixture(t *testing.T) lateBasisFixture {
	t.Helper()
	base := "the basis of a late delta: alpha beta gamma delta epsilon zeta\n"
	evil, err := delta.Create([]byte(base), []byte(base+"evil\n"))
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	return lateBasisFixture{base: base, baseUUID: hash.SHA1([]byte(base)),
		claimed: hash.SHA1([]byte(base + "good\n")), evil: evil}
}

// rows counts uuid's blob rows: all, and the phantoms among them.
func rows(t *testing.T, r *repo.Repo, uuid string) (all, phantoms int) {
	t.Helper()
	if err := r.DB().QueryRow(
		"SELECT count(*), count(CASE WHEN size<0 THEN 1 END) FROM blob WHERE uuid=?",
		uuid).Scan(&all, &phantoms); err != nil {
		t.Fatalf("count %s: %v", uuid, err)
	}
	return all, phantoms
}

// inPass runs fn as one verification pass, in its transaction (runPass): a
// late hash mismatch rolls back everything fn stored.
func inPass(r *repo.Repo, fn func(r *repo.Repo, pass *receivePass) error) error {
	cardErr, err := runPass(r, func(bound *repo.Repo) (error, error) {
		return fn(bound, newReceivePass()), nil
	})
	return errors.Join(cardErr, err)
}

// storeAll stores each of stores in order, as the cards of one pass.
func storeAll(r *repo.Repo, pass *receivePass, stores ...lateStore) error {
	for _, s := range stores {
		if err := storeReceivedFile(context.Background(), r, pass, s.uuid, s.src,
			s.payload, nil, visibilityPublic, nil); err != nil {
			return err
		}
	}
	return nil
}

// lateStore is one received card: an artifact, its delta basis, its payload.
type lateStore struct {
	uuid, src string
	payload   []byte
}

// wantLateMismatch checks err is the late hash mismatch naming uuid, which
// aborts its pass.
func wantLateMismatch(t *testing.T, err error, uuid string) {
	t.Helper()
	var mismatch *hashMismatchError
	if !errors.As(err, &mismatch) || mismatch.uuid != uuid || !abortsPass(err) {
		t.Fatalf("pass = %v, want a late hash mismatch naming %s", err, uuid)
	}
	if !errors.Is(err, errHashMismatch) {
		t.Fatalf("pass = %v, want errors.Is errHashMismatch", err)
	}
}

// TestReceivePassLateBasisMismatch: a delta stored dangling is checked when
// its basis arrives later in the same pass (Section 7.7, SP-7.7-4). The
// mismatch aborts the pass, which keeps neither the delta nor the basis.
func TestReceivePassLateBasisMismatch(t *testing.T) {
	r := setupSyncTestRepo(t)
	fx := newLateBasisFixture(t)
	err := inPass(r, func(r *repo.Repo, pass *receivePass) error {
		return storeAll(r, pass, lateStore{fx.claimed, fx.baseUUID, fx.evil},
			lateStore{fx.baseUUID, "", []byte(fx.base)})
	})
	wantLateMismatch(t, err, fx.claimed)
	for _, uuid := range []string{fx.claimed, fx.baseUUID} {
		if all, _ := rows(t, r, uuid); all != 0 {
			t.Fatalf("%d rows for %s, want 0", all, uuid)
		}
	}
	var deltas int
	r.DB().QueryRow("SELECT count(*) FROM delta").Scan(&deltas)
	if deltas != 0 {
		t.Fatalf("%d delta rows left, want 0", deltas)
	}
}

// TestReceivePassLateBasisKeepsPhantom: a delta that filled a phantom leaves
// it the phantom it was, with its privacy, when its pass aborts.
func TestReceivePassLateBasisKeepsPhantom(t *testing.T) {
	r := setupSyncTestRepo(t)
	fx := newLateBasisFixture(t)
	rid, err := blob.StorePhantom(r.DB(), fx.claimed)
	if err != nil {
		t.Fatalf("StorePhantom: %v", err)
	}
	if err := content.MakePrivate(r.DB(), int64(rid)); err != nil {
		t.Fatalf("MakePrivate: %v", err)
	}
	err = inPass(r, func(r *repo.Repo, pass *receivePass) error {
		return storeAll(r, pass, lateStore{fx.claimed, fx.baseUUID, fx.evil},
			lateStore{fx.baseUUID, "", []byte(fx.base)})
	})
	wantLateMismatch(t, err, fx.claimed)
	if all, phantoms := rows(t, r, fx.claimed); all != 1 || phantoms != 1 {
		t.Fatalf("delta rows = %d (%d phantom), want one phantom", all, phantoms)
	}
	var inPhantom int
	r.DB().QueryRow("SELECT count(*) FROM phantom WHERE rid=?", rid).Scan(&inPhantom)
	if inPhantom != 1 || !content.IsPrivate(r.DB(), int64(rid)) {
		t.Fatalf("phantom row %d, private %v: want a private phantom still", inPhantom,
			content.IsPrivate(r.DB(), int64(rid)))
	}
}

// TestReceivePassLateBasisChain: a dangling delta grounded by another that
// is itself grounded later is checked once the chain reaches its root, and
// its mismatch aborts the pass, good middle delta and basis included.
func TestReceivePassLateBasisChain(t *testing.T) {
	r := setupSyncTestRepo(t)
	fx := newLateBasisFixture(t)
	mid := fx.base + "mid\n"
	top := mid + "top\n"
	midDelta, err := delta.Create([]byte(fx.base), []byte(mid))
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	badTop, err := delta.Create([]byte(mid), []byte(top+"evil\n"))
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	midUUID, topUUID := hash.SHA1([]byte(mid)), hash.SHA1([]byte(top))
	err = inPass(r, func(r *repo.Repo, pass *receivePass) error {
		return storeAll(r, pass, lateStore{topUUID, midUUID, badTop},
			lateStore{midUUID, fx.baseUUID, midDelta},
			lateStore{fx.baseUUID, "", []byte(fx.base)})
	})
	wantLateMismatch(t, err, topUUID)
	for _, uuid := range []string{topUUID, midUUID, fx.baseUUID} {
		if all, _ := rows(t, r, uuid); all != 0 {
			t.Fatalf("%d rows for %s, want 0", all, uuid)
		}
	}
}

// TestReceivePassHeldChainLateBasis: a bad delta whose basis is a delta row
// stored in an earlier pass, against a phantom that arrives in this one, is
// still checked (SP-7.7-4): the chain to its bytes runs through a row this
// pass did not store. The abort keeps the earlier pass's row.
func TestReceivePassHeldChainLateBasis(t *testing.T) {
	r := setupSyncTestRepo(t)
	fx := newLateBasisFixture(t)
	mid := fx.base + "mid\n"
	midDelta, err := delta.Create([]byte(fx.base), []byte(mid))
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	midUUID := hash.SHA1([]byte(mid))
	// An earlier pass left mid as a delta on the phantom basis.
	if err := inPass(r, func(r *repo.Repo, pass *receivePass) error {
		return storeAll(r, pass, lateStore{midUUID, fx.baseUUID, midDelta})
	}); err != nil {
		t.Fatalf("store held delta: %v", err)
	}
	badTop, err := delta.Create([]byte(mid), []byte(mid+"evil\n"))
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	claimed := hash.SHA1([]byte(mid + "good\n"))
	err = inPass(r, func(r *repo.Repo, pass *receivePass) error {
		return storeAll(r, pass, lateStore{claimed, midUUID, badTop},
			lateStore{fx.baseUUID, "", []byte(fx.base)})
	})
	wantLateMismatch(t, err, claimed)
	if all, _ := rows(t, r, claimed); all != 0 {
		t.Fatalf("%d rows for the mismatched delta, want 0", all)
	}
	if all, phantoms := rows(t, r, midUUID); all != 1 || phantoms != 0 {
		t.Fatalf("held middle rows = %d (%d phantom), want it kept", all, phantoms)
	}
	if _, ok := content.AvailableByUUID(r.DB(), fx.baseUUID); ok {
		t.Fatal("the basis of the aborted pass is stored")
	}
}

// TestReceivePassLateBasisChainIsLinear: the late check of a long chain
// that arrives newest first, as a clone's does, applies each delta once. A
// check that rebuilt every delta from the basis would take the square of the
// chain length, which at this length is far past the bound.
func TestReceivePassLateBasisChainIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 3000-delta chain")
	}
	const n = 3000
	r := setupSyncTestRepo(t)
	ctx := context.Background()
	revs := make([][]byte, n+1)
	pad := make([]byte, 5000)
	for i := range pad {
		pad[i] = byte('a' + i*7%26)
	}
	for i := range revs {
		revs[i] = append(append([]byte{}, pad...), fmt.Sprintf("\nrevision %d\n", i)...)
	}
	var took time.Duration
	err := inPass(r, func(r *repo.Repo, pass *receivePass) error {
		for i := n; i >= 1; i-- {
			d, err := delta.Create(revs[i-1], revs[i])
			if err != nil {
				t.Fatalf("delta.Create %d: %v", i, err)
			}
			if err := storeReceivedFile(ctx, r, pass, hash.SHA1(revs[i]),
				hash.SHA1(revs[i-1]), d, nil, visibilityPublic, nil); err != nil {
				return fmt.Errorf("store delta %d: %w", i, err)
			}
		}
		start := time.Now()
		if err := storeReceivedFile(ctx, r, pass, hash.SHA1(revs[0]), "", revs[0], nil,
			visibilityPublic, nil); err != nil {
			return fmt.Errorf("store basis: %w", err)
		}
		took = time.Since(start)
		if len(pass.dangling) != 0 {
			return fmt.Errorf("%d deltas still dangling", len(pass.dangling))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if took > 20*time.Second {
		t.Fatalf("checking a %d-delta chain took %v, want linear time", n, took)
	}
	if _, ok := content.AvailableByUUID(r.DB(), hash.SHA1(revs[n])); !ok {
		t.Fatal("the chain's newest delta is not available after the pass")
	}
}

// TestReceivePassChecksAChainFromItsBasis pins the cost of a chain of deltas
// received basis first, each against the one before, as a clone of a real
// repository receives a file's history: checking each delta must start from
// its basis's bytes the pass already holds, not rebuild the basis from the
// chain's root. Rebuilding made a dense clone of the Fossil repository
// (chains averaging 532 deltas) quadratic, past an hour of CPU.
func TestReceivePassChecksAChainFromItsBasis(t *testing.T) {
	r := setupSyncTestRepo(t)
	const links = 50
	err := inPass(r, func(r *repo.Repo, pass *receivePass) error {
		prev := []byte("version 0 of a file whose history is one chain\n")
		prevUUID := hash.SHA1(prev)
		if err := storeAll(r, pass, lateStore{uuid: prevUUID, payload: prev}); err != nil {
			return err
		}
		for i := 1; i <= links; i++ {
			next := append(append([]byte(nil), prev...), fmt.Sprintf("line %d\n", i)...)
			d, err := delta.Create(prev, next)
			if err != nil {
				return err
			}
			nextUUID := hash.SHA1(next)
			if err := storeAll(r, pass, lateStore{nextUUID, prevUUID, d}); err != nil {
				return err
			}
			prev, prevUUID = next, nextUUID
		}
		if s := pass.basis.Stats(); s.Misses != 0 || s.Hits != links {
			return fmt.Errorf("basis lookups: %d hits, %d misses; want %d hits, 0 misses",
				s.Hits, s.Misses, links)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestReceivePassAvailable checks the pass's availability answer for a delta
// basis: a basis whose bytes the pass holds is available without a walk, one
// dangling in the pass is not, and any other is asked of the repository.
func TestReceivePassAvailable(t *testing.T) {
	r := setupSyncTestRepo(t)
	f := newLateBasisFixture(t)
	err := inPass(r, func(r *repo.Repo, pass *receivePass) error {
		good, err := delta.Create([]byte(f.base), []byte(f.base+"good\n"))
		if err != nil {
			return err
		}
		if err := storeAll(r, pass, lateStore{f.claimed, f.baseUUID, good}); err != nil {
			return err
		}
		other := hash.SHA1([]byte("never sent"))
		for _, tt := range []struct {
			uuid string
			want bool
		}{{f.claimed, false}, {f.baseUUID, false}, {other, false}} {
			if got := pass.available(r.DB(), tt.uuid); got != tt.want {
				return fmt.Errorf("available(%s) before the basis = %v, want %v",
					tt.uuid, got, tt.want)
			}
		}
		if err := storeAll(r, pass, lateStore{uuid: f.baseUUID, payload: []byte(f.base)}); err != nil {
			return err
		}
		for _, uuid := range []string{f.claimed, f.baseUUID} {
			if !pass.available(r.DB(), uuid) {
				return fmt.Errorf("available(%s) after the basis = false", uuid)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A row whose content was dropped (a seed seedcheck refused) is no basis,
// even while the pass's cache still holds the bytes it had: a delta on it is
// not verified against bytes the store no longer keeps.
func TestReceivePassDroppedBasisUnverifiable(t *testing.T) {
	r := setupSyncTestRepo(t)
	base := []byte("a basis whose content a seed verdict drops\n")
	err := inPass(r, func(r *repo.Repo, pass *receivePass) error {
		rid, uuid, err := blob.Store(r.DB(), base)
		if err != nil {
			return err
		}
		pass.remember(rid, base)
		if _, err := r.DB().Exec("UPDATE blob SET size=-1, content=NULL WHERE rid=?", rid); err != nil {
			return err
		}
		target := append([]byte("on the dropped row\n"), base...)
		d, err := delta.Create(base, target)
		if err != nil {
			return err
		}
		verdict, got, err := pass.checkDelta(r.DB(), hash.SHA1(target), uuid, d)
		if err != nil {
			return err
		}
		if verdict != deltaUnverifiable || got != nil {
			return fmt.Errorf("a delta on a dropped row was judged %v", verdict)
		}
		if pass.available(r.DB(), uuid) {
			return fmt.Errorf("the dropped row reads as available")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
