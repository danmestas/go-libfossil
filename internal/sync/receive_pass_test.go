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

// TestReceivePassLateBasisMismatch: a delta stored dangling is checked when
// its basis arrives later in the same pass (Section 7.7, SP-7.7-4). The
// basis's store reports the delta's name as a hash mismatch, the basis
// stays stored, and the delta, new in the pass, is gone.
func TestReceivePassLateBasisMismatch(t *testing.T) {
	r := setupSyncTestRepo(t)
	fx := newLateBasisFixture(t)
	ctx := context.Background()
	pass := newReceivePass()
	if err := storeReceivedFile(ctx, r, pass, fx.claimed, fx.baseUUID, fx.evil, nil,
		visibilityPublic, nil); err != nil {
		t.Fatalf("store delta before basis: %v", err)
	}
	err := storeReceivedFile(ctx, r, pass, fx.baseUUID, "", []byte(fx.base), nil,
		visibilityPublic, nil)
	var mismatch *hashMismatchError
	if !errors.As(err, &mismatch) || mismatch.uuid != fx.claimed {
		t.Fatalf("store basis = %v, want a hash mismatch naming %s", err, fx.claimed)
	}
	if !errors.Is(err, errHashMismatch) {
		t.Fatalf("store basis = %v, want errors.Is errHashMismatch", err)
	}
	if all, _ := rows(t, r, fx.claimed); all != 0 {
		t.Fatalf("%d rows for the mismatched delta, want 0", all)
	}
	if _, ok := content.AvailableByUUID(r.DB(), fx.baseUUID); !ok {
		t.Fatal("the basis is not stored")
	}
	var deltas int
	r.DB().QueryRow("SELECT count(*) FROM delta").Scan(&deltas)
	if deltas != 0 {
		t.Fatalf("%d delta rows left, want 0", deltas)
	}
}

// TestReceivePassLateBasisRestoresPhantom: a delta that filled a phantom is
// returned to a phantom, with its privacy, so what referred to it still
// resolves.
func TestReceivePassLateBasisRestoresPhantom(t *testing.T) {
	r := setupSyncTestRepo(t)
	fx := newLateBasisFixture(t)
	ctx := context.Background()
	rid, err := blob.StorePhantom(r.DB(), fx.claimed)
	if err != nil {
		t.Fatalf("StorePhantom: %v", err)
	}
	if err := content.MakePrivate(r.DB(), int64(rid)); err != nil {
		t.Fatalf("MakePrivate: %v", err)
	}
	pass := newReceivePass()
	if err := storeReceivedFile(ctx, r, pass, fx.claimed, fx.baseUUID, fx.evil, nil,
		visibilityPublic, nil); err != nil {
		t.Fatalf("store delta before basis: %v", err)
	}
	err = storeReceivedFile(ctx, r, pass, fx.baseUUID, "", []byte(fx.base), nil,
		visibilityPublic, nil)
	if !errors.Is(err, errHashMismatch) {
		t.Fatalf("store basis = %v, want a hash mismatch", err)
	}
	if all, phantoms := rows(t, r, fx.claimed); all != 1 || phantoms != 1 {
		t.Fatalf("delta rows = %d (%d phantom), want one phantom", all, phantoms)
	}
	var inPhantom int
	r.DB().QueryRow("SELECT count(*) FROM phantom WHERE rid=?", rid).Scan(&inPhantom)
	if inPhantom != 1 || !content.IsPrivate(r.DB(), int64(rid)) {
		t.Fatalf("phantom row %d, private %v: want a private phantom again", inPhantom,
			content.IsPrivate(r.DB(), int64(rid)))
	}
}

// TestReceivePassLateBasisChain: a dangling delta grounded by another that
// is itself grounded later is checked once the chain reaches its root, and
// a good one is kept.
func TestReceivePassLateBasisChain(t *testing.T) {
	r := setupSyncTestRepo(t)
	fx := newLateBasisFixture(t)
	ctx := context.Background()
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
	pass := newReceivePass()
	midUUID, topUUID := hash.SHA1([]byte(mid)), hash.SHA1([]byte(top))
	for _, s := range []struct {
		uuid, src string
		payload   []byte
	}{{topUUID, midUUID, badTop}, {midUUID, fx.baseUUID, midDelta}} {
		if err := storeReceivedFile(ctx, r, pass, s.uuid, s.src, s.payload, nil,
			visibilityPublic, nil); err != nil {
			t.Fatalf("store %s: %v", s.uuid, err)
		}
	}
	err = storeReceivedFile(ctx, r, pass, fx.baseUUID, "", []byte(fx.base), nil,
		visibilityPublic, nil)
	var mismatch *hashMismatchError
	if !errors.As(err, &mismatch) || mismatch.uuid != topUUID {
		t.Fatalf("store basis = %v, want a hash mismatch naming the top", err)
	}
	if all, _ := rows(t, r, topUUID); all != 0 {
		t.Fatalf("%d rows for the bad top, want 0", all)
	}
	midRid, ok := content.AvailableByUUID(r.DB(), midUUID)
	if !ok {
		t.Fatal("the good middle delta is not available")
	}
	if got, err := content.Expand(r.DB(), midRid); err != nil || string(got) != mid {
		t.Fatalf("expand middle = %q, %v", got, err)
	}
	if len(pass.dangling) != 0 || len(pass.byRoot) != 0 {
		t.Fatalf("pass still tracks %d deltas, %d sources", len(pass.dangling),
			len(pass.byRoot))
	}
}

// TestReceivePassHeldChainLateBasis: a bad delta whose basis is a delta row
// stored in an earlier pass, against a phantom that arrives in this one, is
// still checked (SP-7.7-4): the chain to its bytes runs through a row this
// pass did not store.
func TestReceivePassHeldChainLateBasis(t *testing.T) {
	r := setupSyncTestRepo(t)
	fx := newLateBasisFixture(t)
	ctx := context.Background()
	mid := fx.base + "mid\n"
	midDelta, err := delta.Create([]byte(fx.base), []byte(mid))
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	midUUID := hash.SHA1([]byte(mid))
	// An earlier pass left mid as a delta on the phantom basis.
	if err := storeReceivedFile(ctx, r, newReceivePass(), midUUID, fx.baseUUID, midDelta,
		nil, visibilityPublic, nil); err != nil {
		t.Fatalf("store held delta: %v", err)
	}
	badTop, err := delta.Create([]byte(mid), []byte(mid+"evil\n"))
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	claimed := hash.SHA1([]byte(mid + "good\n"))
	pass := newReceivePass()
	if err := storeReceivedFile(ctx, r, pass, claimed, midUUID, badTop, nil,
		visibilityPublic, nil); err != nil {
		t.Fatalf("store dangling delta: %v", err)
	}
	err = storeReceivedFile(ctx, r, pass, fx.baseUUID, "", []byte(fx.base), nil,
		visibilityPublic, nil)
	var mismatch *hashMismatchError
	if !errors.As(err, &mismatch) || mismatch.uuid != claimed {
		t.Fatalf("store basis = %v, want a hash mismatch naming %s", err, claimed)
	}
	if all, _ := rows(t, r, claimed); all != 0 {
		t.Fatalf("%d rows for the mismatched delta, want 0", all)
	}
	if _, ok := content.AvailableByUUID(r.DB(), midUUID); !ok {
		t.Fatal("the held middle delta is not available")
	}
}

// TestReceivePassKeepsDependentsOfRevertedDelta: when a delta is reverted, the
// dangling deltas that were stored on it wait on it again, and are checked
// when its real content arrives.
func TestReceivePassKeepsDependentsOfRevertedDelta(t *testing.T) {
	r := setupSyncTestRepo(t)
	fx := newLateBasisFixture(t)
	ctx := context.Background()
	mid := fx.base + "mid\n"
	top := mid + "top\n"
	badMid, err := delta.Create([]byte(fx.base), []byte(fx.base+"evil\n"))
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	topDelta, err := delta.Create([]byte(mid), []byte(top))
	if err != nil {
		t.Fatalf("delta.Create: %v", err)
	}
	midUUID, topUUID := hash.SHA1([]byte(mid)), hash.SHA1([]byte(top))
	pass := newReceivePass()
	for _, s := range []struct {
		uuid, src string
		payload   []byte
	}{{topUUID, midUUID, topDelta}, {midUUID, fx.baseUUID, badMid}} {
		if err := storeReceivedFile(ctx, r, pass, s.uuid, s.src, s.payload, nil,
			visibilityPublic, nil); err != nil {
			t.Fatalf("store %s: %v", s.uuid, err)
		}
	}
	err = storeReceivedFile(ctx, r, pass, fx.baseUUID, "", []byte(fx.base), nil,
		visibilityPublic, nil)
	var mismatch *hashMismatchError
	if !errors.As(err, &mismatch) || mismatch.uuid != midUUID {
		t.Fatalf("store basis = %v, want a hash mismatch naming the middle", err)
	}
	if err := storeReceivedFile(ctx, r, pass, midUUID, "", []byte(mid), nil,
		visibilityPublic, nil); err != nil {
		t.Fatalf("store the real middle: %v", err)
	}
	rid, ok := content.AvailableByUUID(r.DB(), topUUID)
	if !ok {
		t.Fatal("the top delta is not available after its basis arrived")
	}
	if got, err := content.Expand(r.DB(), rid); err != nil || string(got) != top {
		t.Fatalf("expand top = %q, %v", got, err)
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
	pass := newReceivePass()
	for i := n; i >= 1; i-- {
		d, err := delta.Create(revs[i-1], revs[i])
		if err != nil {
			t.Fatalf("delta.Create %d: %v", i, err)
		}
		if err := storeReceivedFile(ctx, r, pass, hash.SHA1(revs[i]),
			hash.SHA1(revs[i-1]), d, nil, visibilityPublic, nil); err != nil {
			t.Fatalf("store delta %d: %v", i, err)
		}
	}
	start := time.Now()
	if err := storeReceivedFile(ctx, r, pass, hash.SHA1(revs[0]), "", revs[0], nil,
		visibilityPublic, nil); err != nil {
		t.Fatalf("store basis: %v", err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("checking a %d-delta chain took %v, want linear time", n, took)
	}
	if len(pass.dangling) != 0 {
		t.Fatalf("%d deltas still dangling", len(pass.dangling))
	}
}
