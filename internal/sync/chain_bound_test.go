package sync

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// chainLink is the content of link i of a planted chain.
func chainLink(i int) []byte { return fmt.Appendf(nil, "chain link %08d\n", i) }

// plantRealChain stores links 0..n-1 of a chain, link i a delta on link
// i+1, link n-1 a delta on link n, held as full text or left a phantom, and
// returns the top link's rid and the root's content. Every link expands
// once the root is held, so a walk over the chain reads real bytes.
func plantRealChain(t *testing.T, r *repo.Repo, n int, rootHeld bool) (libfossil.FslID, []byte) {
	t.Helper()
	root := chainLink(n)
	var top libfossil.FslID
	err := r.WithTx(func(tx *db.Tx) error {
		src, err := blob.StorePhantom(tx, hash.SHA1(root))
		if rootHeld {
			src, _, err = blob.Store(tx, root)
		}
		if err != nil {
			return err
		}
		for i := n - 1; i >= 0; i-- {
			d, err := delta.Create(chainLink(i+1), chainLink(i))
			if err != nil {
				return err
			}
			rid, err := blob.StoreDeltaRaw(tx, hash.SHA1(chainLink(i)), d, src, nil)
			if err != nil {
				return err
			}
			src, top = rid, rid
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return top, root
}

// A delta pushed to close a two-delta cycle is refused at once, rather than
// after a walk of 1<<24 steps that held the write lock half a minute.
func TestCyclicDeltaPushRefusedQuickly(t *testing.T) {
	o := newPCOrigin(t)
	_, a := o.labelled("u", "one side of a cycle")
	_, b := o.labelled("u", "the other side")
	x := deltaCard(t, b, hash.SHA1(b), a)
	y := deltaCard(t, a, hash.SHA1(a), b)
	if errs := errorsOf(o.push(x)); len(errs) != 0 {
		t.Fatalf("the first delta: errors %q", errs)
	}
	start := time.Now()
	errs := errorsOf(o.push(y))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("closing the cycle took %v", elapsed)
	}
	if len(errs) == 0 {
		t.Fatal("the delta closing a cycle was stored")
	}
}

// A delta pushed onto a chain longer than content.MaxDeltaChainDepth can
// never be expanded, but it is an honest artifact: the push stores it, as a
// push onto any chain deeper than the bound always did.
func TestOverDeepDeltaPushStored(t *testing.T) {
	o := newPCOrigin(t)
	held := []byte("unused\n")
	phantom, err := blob.StorePhantom(o.r.DB(), hash.SHA3(held))
	if err != nil {
		t.Fatal(err)
	}
	top := plantDeepChain(t, o.r, phantom, content.MaxDeltaChainDepth+5)
	var topUUID string
	if err := o.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", top).Scan(&topUUID); err != nil {
		t.Fatal(err)
	}
	_, onTop := o.labelled("u", "an artifact pushed onto the deep chain")
	d := deltaCard(t, []byte("whatever the top link holds\n"), topUUID, onTop)
	if errs := errorsOf(o.push(d)); len(errs) != 0 {
		t.Fatalf("the push onto an over-deep chain was refused: %q", errs)
	}
	if _, ok := blob.Exists(o.r.DB(), d.UUID); !ok {
		t.Fatal("the delta was not stored")
	}
}

// A dangling delta whose path to the basis that arrives runs past
// content.MaxDeltaChainDepth is not checked against its name when the
// basis arrives: no read can expand it, so the pass owes it nothing. One
// within the bound on the same basis is checked as before.
func TestOverDeepDanglingDeltaNotVerified(t *testing.T) {
	o := newPCOrigin(t)
	top, root := plantRealChain(t, o.r, content.MaxDeltaChainDepth-3, false)
	var topUUID string
	if err := o.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", top).Scan(&topUUID); err != nil {
		t.Fatal(err)
	}
	r1Bytes := []byte("r1 pushed onto the planted chain\n")
	r1 := deltaCard(t, chainLink(0), topUUID, r1Bytes)
	d0Bytes := []byte("d0 pushed onto r1\n")
	d0 := deltaCard(t, r1Bytes, r1.UUID, d0Bytes)
	// d1 does not hash to its name; its path is one node past the bound.
	d1 := deltaCard(t, d0Bytes, d0.UUID, []byte("d1 pushed onto d0\n"))
	d1.UUID = hash.SHA1([]byte("a name d1's bytes do not have\n"))
	cards := []xfer.Card{d1, d0, r1, &xfer.FileCard{UUID: hash.SHA1(root), Content: root}}
	if errs := errorsOf(o.push(cards...)); len(errs) != 0 {
		t.Fatalf("the push was refused: %q", errs)
	}
	for _, u := range []string{d0.UUID, r1.UUID} {
		rid, _ := blob.Exists(o.r.DB(), u)
		if !content.IsAvailable(o.r.DB(), rid) {
			t.Fatalf("%.10s within the bound is not readable", u)
		}
	}
}

// chainEnd stops at its limit: a chain one node longer is errChainTooDeep,
// one exactly as long has its root.
func TestChainEndStopsAtLimit(t *testing.T) {
	o := newPCOrigin(t)
	phantom, err := blob.StorePhantom(o.r.DB(), hash.SHA3([]byte("the root\n")))
	if err != nil {
		t.Fatal(err)
	}
	const limit = 64
	top := plantDeepChain(t, o.r, phantom, limit)
	if _, err := chainEnd(o.r.DB(), top, nil, limit); !errors.Is(err, errChainTooDeep) {
		t.Fatalf("a chain of %d nodes under limit %d: %v, want errChainTooDeep", limit+1, limit, err)
	}
	if root, err := chainEnd(o.r.DB(), top, nil, limit+1); err != nil || root != phantom {
		t.Fatalf("a chain of %d nodes under limit %d: root %d (%v), want %d", limit+1, limit+1, root, err, phantom)
	}
}

// The same when the basis that makes the dangling deltas readable is itself
// a delta deep on a held chain: their depth counts that chain too.
func TestOverDeepDanglingDeltaOnDeepBasisNotVerified(t *testing.T) {
	o := newPCOrigin(t)
	top, _ := plantRealChain(t, o.r, content.MaxDeltaChainDepth-3, true)
	var topUUID string
	if err := o.r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", top).Scan(&topUUID); err != nil {
		t.Fatal(err)
	}
	r1Bytes := []byte("r1 pushed onto the held chain\n")
	r1 := deltaCard(t, chainLink(0), topUUID, r1Bytes)
	d0Bytes := []byte("d0 pushed onto r1\n")
	d0 := deltaCard(t, r1Bytes, r1.UUID, d0Bytes)
	d1 := deltaCard(t, d0Bytes, d0.UUID, []byte("d1 pushed onto d0\n"))
	d1.UUID = hash.SHA1([]byte("a name d1's bytes do not have\n"))
	if errs := errorsOf(o.push(d1, d0, r1)); len(errs) != 0 {
		t.Fatalf("the push was refused: %q", errs)
	}
	rid, _ := blob.Exists(o.r.DB(), d0.UUID)
	if !content.IsAvailable(o.r.DB(), rid) {
		t.Fatal("d0, within the bound, is not readable")
	}
}
