package content

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// inTx runs fn against a *db.Tx, which Deltify and Undelta require. All
// assertions happen inside the transaction, so nothing here depends on
// commit behaviour.
func inTx(t *testing.T, fn func(tx *db.Tx)) {
	t.Helper()
	d := setupTestDB(t)
	if err := d.WithTx(func(tx *db.Tx) error {
		fn(tx)
		return nil
	}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

// similarPair returns two bodies that share almost all of their text, so a
// delta between them is far below the 75% policy threshold.
func similarPair() ([]byte, []byte) {
	var a, b bytes.Buffer
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&a, "line %04d: the quick brown fox jumps over the lazy dog\n", i)
		if i == 100 {
			fmt.Fprintf(&b, "line %04d: CHANGED\n", i)
		} else {
			fmt.Fprintf(&b, "line %04d: the quick brown fox jumps over the lazy dog\n", i)
		}
	}
	return a.Bytes(), b.Bytes()
}

func isDelta(t *testing.T, q db.Querier, rid libfossil.FslID) bool {
	t.Helper()
	src, err := DeltaSource(q, rid)
	if err != nil {
		t.Fatalf("DeltaSource(%d): %v", rid, err)
	}
	return src > 0
}

func TestDeltifyEncodesSimilarPredecessor(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		v1, v2 := similarPair()

		oldRid, _, err := blob.Store(tx, v1)
		if err != nil {
			t.Fatalf("store v1: %v", err)
		}
		newRid, _, err := blob.Store(tx, v2)
		if err != nil {
			t.Fatalf("store v2: %v", err)
		}

		saved, err := Deltify(tx, oldRid, newRid)
		if err != nil {
			t.Fatalf("Deltify: %v", err)
		}
		if saved <= 0 {
			t.Fatalf("expected a positive saving, got %d", saved)
		}
		if !isDelta(t, tx, oldRid) {
			t.Error("predecessor was not delta-encoded")
		}
		if isDelta(t, tx, newRid) {
			t.Error("the new artifact must stay whole: deltification runs backwards")
		}

		got, err := Expand(tx, oldRid)
		if err != nil {
			t.Fatalf("Expand(old): %v", err)
		}
		if !bytes.Equal(got, v1) {
			t.Error("delta-encoded artifact did not expand to its original content")
		}
	})
}

func TestDeltifyLeavesAlreadyDeltaAlone(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		v1, v2 := similarPair()
		v3 := append(bytes.Clone(v2), []byte("trailing addition\n")...)

		r1, _, _ := blob.Store(tx, v1)
		r2, _, _ := blob.Store(tx, v2)
		r3, _, _ := blob.Store(tx, v3)

		if _, err := Deltify(tx, r1, r2); err != nil {
			t.Fatalf("Deltify(r1,r2): %v", err)
		}
		// r1 is now a delta of r2. Re-offering it must be declined: this is
		// the rule that keeps chain depth linear rather than compounding.
		saved, err := Deltify(tx, r1, r3)
		if err != nil {
			t.Fatalf("Deltify(r1,r3): %v", err)
		}
		if saved != 0 {
			t.Errorf("expected an already-delta artifact to be declined, saved=%d", saved)
		}
		src, _ := DeltaSource(tx, r1)
		if src != r2 {
			t.Errorf("delta source moved from %d to %d", r2, src)
		}
	})
}

func TestDeltifyDeclinesTinyTarget(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		// Target under 50 bytes, source comfortably over it, so only the
		// target-side floor (content.c:881) can decline this pair.
		tiny, _, _ := blob.Store(tx, []byte("tiny target content"))
		big, _, _ := blob.Store(tx, []byte(strings.Repeat("tiny target content and more\n", 40)))

		saved, err := Deltify(tx, tiny, big)
		if err != nil {
			t.Fatalf("Deltify: %v", err)
		}
		if saved != 0 {
			t.Errorf("a target under %d bytes must be left whole, saved=%d", deltifyMinBytes, saved)
		}
		if isDelta(t, tx, tiny) {
			t.Error("tiny target was delta-encoded")
		}
	})
}

// TestDeltifyDeclinesTinySource covers the source-side floor at
// content.c:911, which the tiny-target case above cannot reach: that one
// returns at the target check first.
func TestDeltifyDeclinesTinySource(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		// The target clears 50 bytes comfortably, so the source-side floor
		// is the only rule that can decline this pair.
		big, _, _ := blob.Store(tx, []byte(strings.Repeat("shared prefix line for the delta test\n", 40)))
		tiny, _, _ := blob.Store(tx, []byte("short\n"))

		saved, err := Deltify(tx, big, tiny)
		if err != nil {
			t.Fatalf("Deltify: %v", err)
		}
		if saved != 0 {
			t.Errorf("a source under %d bytes must not be used, saved=%d", deltifyMinBytes, saved)
		}
		if isDelta(t, tx, big) {
			t.Error("artifact was delta-encoded against a sub-50-byte source")
		}
	})
}

// TestDeltifyDeclinesDissimilarContent pins the 75% ratio rule
// (content.c:917): a delta that does not save at least a quarter of the
// target is not worth the indirection on every read. The two bodies below
// share no runs long enough for the delta encoder to exploit.
func TestDeltifyDeclinesDissimilarContent(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		var a, b bytes.Buffer
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&a, "alpha %04d aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", i)
			fmt.Fprintf(&b, "%04d zzzz beta gamma delta epsilon zeta\n", i*7919)
		}
		r1, _, _ := blob.Store(tx, a.Bytes())
		r2, _, _ := blob.Store(tx, b.Bytes())

		saved, err := Deltify(tx, r1, r2)
		if err != nil {
			t.Fatalf("Deltify: %v", err)
		}
		if saved != 0 {
			t.Errorf("dissimilar pair must be declined, saved=%d", saved)
		}
		if isDelta(t, tx, r1) {
			t.Error("dissimilar pair was delta-encoded; the 75% ratio rule did not fire")
		}
	})
}

func TestDeltifyRefusesPrivateIntoPublic(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		v1, v2 := similarPair()
		pub, _, _ := blob.Store(tx, v1)
		priv, _, _ := blob.Store(tx, v2)
		if err := MakePrivate(tx, int64(priv)); err != nil {
			t.Fatalf("MakePrivate: %v", err)
		}

		saved, err := Deltify(tx, pub, priv)
		if err != nil {
			t.Fatalf("Deltify: %v", err)
		}
		if saved != 0 {
			t.Errorf("expected private->public to be declined, saved=%d", saved)
		}
		if isDelta(t, tx, pub) {
			t.Error("a public artifact must never be stored as a delta against a private one")
		}
	})
}

// TestDeltifyBreaksLoopAndDeclines pins the exact canonical behaviour at
// content.c:900-908: when rid is already an ancestor of srcRid, the source is
// undeltaed to break the dependency AND the pairing is declined for now.
// Undeltaing but then proceeding would also be loop-free, so the decline is
// the half that only an explicit assertion catches.
func TestDeltifyBreaksLoopAndDeclines(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		v1, v2 := similarPair()
		r1, _, _ := blob.Store(tx, v1)
		r2, _, _ := blob.Store(tx, v2)

		// Make r2 a delta of r1, then ask for the reverse.
		if _, err := Deltify(tx, r2, r1); err != nil {
			t.Fatalf("Deltify(r2,r1): %v", err)
		}
		if !isDelta(t, tx, r2) {
			t.Fatal("setup failed: r2 is not a delta")
		}

		saved, err := Deltify(tx, r1, r2)
		if err != nil {
			t.Fatalf("Deltify(r1,r2): %v", err)
		}
		if isDelta(t, tx, r2) {
			t.Error("source should have been undeltaed to break the loop")
		}
		if saved != 0 {
			t.Errorf("canonical declines the pairing after breaking the loop, saved=%d", saved)
		}
		if isDelta(t, tx, r1) {
			t.Error("target must be left whole: canonical declines rather than proceeding")
		}

		for _, rid := range []libfossil.FslID{r1, r2} {
			if _, err := Expand(tx, rid); err != nil {
				t.Errorf("Expand(%d) after loop break: %v", rid, err)
			}
		}
	})
}

// TestDeltifyBreaksLoopTerminatesOnCycle pins the bound on the ancestor
// walk. blob.StoreDeltaRaw records delta(rid,srcid) links straight from the
// wire with no cycle check, so a sync peer can shape A->B->A; an unbounded
// walk would spin forever inside the write transaction.
//
// This exercises deltifyBreaksLoop directly rather than going through
// Deltify, because Deltify cannot currently reach the walk with a cyclic
// source: IsAvailable runs first on both rids and follows the same delta
// links under maxDeltaChainDepth, so it returns false and Deltify declines
// before the walk begins. That protection is incidental -- IsAvailable is
// answering a different question (groundedness), and reordering or relaxing
// it would expose the walk. Every walk over the peer-shaped delta graph
// carries its own visited set and its own step cap for that reason, so the
// test drives this walk rather than relying on another one's guard.
func TestDeltifyBreaksLoopTerminatesOnCycle(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		v1, v2 := similarPair()
		target, _, _ := blob.Store(tx, append(bytes.Clone(v1), []byte("target\n")...))
		a, _, _ := blob.Store(tx, v1)
		b, _, _ := blob.Store(tx, v2)

		// Forge the cycle directly, the way a hostile peer's deltas land.
		for _, link := range [][2]libfossil.FslID{{a, b}, {b, a}} {
			if _, err := tx.Exec("REPLACE INTO delta(rid, srcid) VALUES(?, ?)", link[0], link[1]); err != nil {
				t.Fatalf("forge delta link: %v", err)
			}
		}

		_, err := deltifyBreaksLoop(tx, target, a)
		if err == nil {
			t.Fatal("expected a cycle to be reported, got success")
		}
		if !strings.Contains(err.Error(), "cycle") {
			t.Errorf("expected a cycle error, got: %v", err)
		}
	})
}

// TestDeltifyDeclinesUngroundedSource records the belt-and-braces behaviour
// the test above documents: a cyclic delta graph reaching Deltify is
// declined, not an error and not a hang.
func TestDeltifyDeclinesUngroundedSource(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		v1, v2 := similarPair()
		target, _, _ := blob.Store(tx, append(bytes.Clone(v1), []byte("target\n")...))
		a, _, _ := blob.Store(tx, v1)
		b, _, _ := blob.Store(tx, v2)

		for _, link := range [][2]libfossil.FslID{{a, b}, {b, a}} {
			if _, err := tx.Exec("REPLACE INTO delta(rid, srcid) VALUES(?, ?)", link[0], link[1]); err != nil {
				t.Fatalf("forge delta link: %v", err)
			}
		}

		saved, err := Deltify(tx, target, a)
		if err != nil {
			t.Fatalf("Deltify on a cyclic source graph: %v", err)
		}
		if saved != 0 {
			t.Errorf("expected an ungrounded source to be declined, saved=%d", saved)
		}
		if isDelta(t, tx, target) {
			t.Error("target was deltified against an ungrounded source")
		}
	})
}

// TestDeltifyBreaksLoopBoundsAtChainDepth pins deltifyBreaksLoop's step cap to
// maxDeltaChainDepth, the same bound the read-path walks use. An acyclic,
// grounded chain one longer than the bound has no cycle for the seen set to
// catch, so only the step cap can stop the walk; if the cap regresses to a
// larger value, this chain runs past maxDeltaChainDepth without erroring.
func TestDeltifyBreaksLoopBoundsAtChainDepth(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		// One node past the bound: the walk from chain[0] visits
		// maxDeltaChainDepth+1 nodes, which the cap must reject.
		n := maxDeltaChainDepth + 2

		rids := make([]libfossil.FslID, n)
		for i := 0; i < n; i++ {
			res, err := tx.Exec(
				"INSERT INTO blob(uuid, size, content, rcvid) VALUES(printf('%040d', ?), 42, x'00', 1)", i)
			if err != nil {
				t.Fatalf("insert blob %d: %v", i, err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				t.Fatalf("LastInsertId: %v", err)
			}
			rids[i] = libfossil.FslID(id)
		}
		// Acyclic chain grounded at its far end: chain[i] deltas against
		// chain[i+1], so deltaSource walks chain[0] -> chain[n-1] and stops.
		for i := 0; i < n-1; i++ {
			if _, err := tx.Exec("INSERT INTO delta(rid, srcid) VALUES(?, ?)", rids[i], rids[i+1]); err != nil {
				t.Fatalf("insert delta %d: %v", i, err)
			}
		}

		// A target rid that is not on the chain, so the walk cannot short
		// out via the next == rid loop check and must run to the cap.
		res, err := tx.Exec(
			"INSERT INTO blob(uuid, size, content, rcvid) VALUES(printf('%040d', ?), 42, x'00', 1)", n)
		if err != nil {
			t.Fatalf("insert target blob: %v", err)
		}
		targetID, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}

		_, err = deltifyBreaksLoop(tx, libfossil.FslID(targetID), rids[0])
		if err == nil {
			t.Fatalf("chain of %d nodes was accepted; the step cap must reject "+
				"a chain longer than maxDeltaChainDepth (%d)", n, maxDeltaChainDepth)
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("expected an exceeds-bound error, got: %v", err)
		}
	})
}

func TestUndeltaRestoresFullContent(t *testing.T) {
	inTx(t, func(tx *db.Tx) {
		v1, v2 := similarPair()
		r1, _, _ := blob.Store(tx, v1)
		r2, _, _ := blob.Store(tx, v2)

		if _, err := Deltify(tx, r1, r2); err != nil {
			t.Fatalf("Deltify: %v", err)
		}
		if err := Undelta(tx, r1); err != nil {
			t.Fatalf("Undelta: %v", err)
		}
		if isDelta(t, tx, r1) {
			t.Error("delta link survived Undelta")
		}
		got, err := Expand(tx, r1)
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		if !bytes.Equal(got, v1) {
			t.Error("content changed across Undelta")
		}

		// Undelta restores blob.size to the full content length, which
		// content_deltify deliberately never touches (content.c:761 vs :935).
		var size int
		if err := tx.QueryRow("SELECT size FROM blob WHERE rid=?", r1).Scan(&size); err != nil {
			t.Fatalf("size: %v", err)
		}
		if size != len(v1) {
			t.Errorf("blob.size = %d after Undelta, want %d", size, len(v1))
		}

		// Undelta on an already-whole artifact is a no-op, not an error.
		if err := Undelta(tx, r1); err != nil {
			t.Errorf("Undelta on whole artifact: %v", err)
		}
	})
}
