package content

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// countingQuerier forwards to an inner Querier and counts the blob-content
// reads, the single statement that dominates delta-chain expansion.
type countingQuerier struct {
	inner      db.Querier
	blobLoads  int
	deltaWalks int
}

func (c *countingQuerier) Exec(q string, args ...any) (sql.Result, error) {
	return c.inner.Exec(q, args...)
}

func (c *countingQuerier) QueryRow(q string, args ...any) *sql.Row {
	switch {
	case strings.HasPrefix(q, "SELECT content, size FROM blob"):
		c.blobLoads++
	case strings.HasPrefix(q, "SELECT srcid FROM delta"):
		c.deltaWalks++
	}
	return c.inner.QueryRow(q, args...)
}

func (c *countingQuerier) Query(q string, args ...any) (*sql.Rows, error) {
	return c.inner.Query(q, args...)
}

// buildDeltaChain stores chainLen versions of a similar body and deltifies
// each one against its successor, so version 0 sits at the deep end of a
// chain rooted at the last version — the shape a real Fossil repository has
// after content_deltify has rewritten older blobs as deltas of newer ones.
func buildDeltaChain(t *testing.T, d *db.DB, chainLen int) ([]libfossil.FslID, [][]byte) {
	t.Helper()

	rids := make([]libfossil.FslID, chainLen)
	bodies := make([][]byte, chainLen)
	for i := range rids {
		var body bytes.Buffer
		for line := 0; line < 300; line++ {
			if line == i {
				fmt.Fprintf(&body, "line %04d: version %d\n", line, i)
			} else {
				fmt.Fprintf(&body, "line %04d: the quick brown fox jumps over the lazy dog\n", line)
			}
		}
		bodies[i] = body.Bytes()
		rid, _, err := blob.Store(d, bodies[i])
		if err != nil {
			t.Fatalf("store version %d: %v", i, err)
		}
		rids[i] = rid
	}

	if err := d.WithTx(func(tx *db.Tx) error {
		for i := 0; i < chainLen-1; i++ {
			if _, err := Deltify(tx, rids[i], rids[i+1]); err != nil {
				return fmt.Errorf("deltify %d against %d: %w", i, i+1, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("build chain: %v", err)
	}

	var deltas int
	if err := d.QueryRow("SELECT count(*) FROM delta").Scan(&deltas); err != nil {
		t.Fatalf("count delta: %v", err)
	}
	if deltas != chainLen-1 {
		t.Fatalf("delta rows = %d, want %d — chain was not built", deltas, chainLen-1)
	}
	return rids, bodies
}

// TestCache_MemoizesChainInteriors pins the property Crosslink depends on:
// walking a delta chain once must leave every node it materialized in the
// cache, so expanding the whole chain costs a number of blob reads linear in
// the chain length rather than quadratic.
func TestCache_MemoizesChainInteriors(t *testing.T) {
	const chainLen = 40

	d := setupTestDB(t)
	rids, bodies := buildDeltaChain(t, d, chainLen)

	q := &countingQuerier{inner: d}
	c := NewCache(8 << 20)

	// Ascending rid order is the order Crosslink sweeps in; rid[0] is the
	// deepest node, so its expansion materializes the entire chain.
	for i, rid := range rids {
		got, err := c.Expand(q, rid)
		if err != nil {
			t.Fatalf("Expand rid %d (version %d): %v", rid, i, err)
		}
		if !bytes.Equal(got, bodies[i]) {
			t.Fatalf("version %d: content mismatch", i)
		}
	}

	// Without interior memoization this is chainLen*(chainLen+1)/2 = 820.
	if q.blobLoads > 2*chainLen {
		t.Fatalf("blob content reads = %d, want <= %d for a %d-node chain",
			q.blobLoads, 2*chainLen, chainLen)
	}
}

// TestCache_ChainInteriorContentIsCorrect checks the memoized interiors are
// served as their own content, not as a neighbour's.
func TestCache_ChainInteriorContentIsCorrect(t *testing.T) {
	const chainLen = 12

	d := setupTestDB(t)
	rids, bodies := buildDeltaChain(t, d, chainLen)

	c := NewCache(8 << 20)
	if _, err := c.Expand(d, rids[0]); err != nil {
		t.Fatalf("prime chain: %v", err)
	}

	for i := chainLen - 1; i >= 0; i-- {
		got, err := c.Expand(d, rids[i])
		if err != nil {
			t.Fatalf("Expand version %d: %v", i, err)
		}
		if !bytes.Equal(got, bodies[i]) {
			t.Fatalf("version %d served wrong content", i)
		}
	}

	// Cached interiors must be independent buffers: mutating a returned
	// slice must not corrupt the next read of the same rid.
	first, err := c.Expand(d, rids[chainLen/2])
	if err != nil {
		t.Fatal(err)
	}
	first[0] ^= 0xFF
	second, err := c.Expand(d, rids[chainLen/2])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second, bodies[chainLen/2]) {
		t.Fatal("mutating a returned slice corrupted the cached interior")
	}
}
