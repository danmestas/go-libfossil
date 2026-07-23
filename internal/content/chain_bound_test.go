package content

import (
	"testing"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// maxDeltaChainDepth's doc says both read-path walks stop after visiting that
// many nodes. They reached the bound a node apart until the loop in
// walkDeltaChain was rewritten to count nodes rather than iterations, which
// meant a chain of exactly maxDeltaChainDepth+1 was unavailable to one walk
// and complete to the other. Nothing else pins the two together: each has its
// own loop, and a future edit to either can reintroduce the skew silently.
func TestChainBoundBitesAtSameLength(t *testing.T) {
	for _, n := range []int{maxDeltaChainDepth, maxDeltaChainDepth + 1} {
		d := setupTestDB(t)

		rids := make([]libfossil.FslID, n)
		for i := 0; i < n; i++ {
			res, err := d.Exec(
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
		// An acyclic chain grounded at its far end, so only the bound can
		// decide the outcome.
		for i := 0; i < n-1; i++ {
			if _, err := d.Exec("INSERT INTO delta(rid, srcid) VALUES(?, ?)", rids[i], rids[i+1]); err != nil {
				t.Fatalf("insert delta %d: %v", i, err)
			}
		}

		available := IsAvailable(d, rids[0])
		_, _, err := walkDeltaChain(d, rids[0], nil)
		walked := err == nil

		if available != walked {
			t.Errorf("chain of %d nodes: IsAvailable=%v but walkDeltaChain succeeded=%v; "+
				"the two walks must reach maxDeltaChainDepth (%d) at the same length",
				n, available, walked, maxDeltaChainDepth)
		}
		if n == maxDeltaChainDepth && !available {
			t.Errorf("chain of exactly maxDeltaChainDepth (%d) nodes was rejected", n)
		}
		if n == maxDeltaChainDepth+1 && available {
			t.Errorf("chain of maxDeltaChainDepth+1 (%d) nodes was accepted", n)
		}
	}
}
