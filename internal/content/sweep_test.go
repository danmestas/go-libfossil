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

type sweepResult struct {
	uuid string
	data []byte
	err  error
}

// collectSweep runs ExpandAll and records what it handed visit, failing the
// test if any blob is visited twice.
func collectSweep(t *testing.T, q db.Querier) map[libfossil.FslID]sweepResult {
	t.Helper()
	got := make(map[libfossil.FslID]sweepResult)
	err := ExpandAll(q, func(rid libfossil.FslID, uuid string, data []byte, err error) error {
		if _, dup := got[rid]; dup {
			t.Fatalf("rid %d visited twice", rid)
		}
		got[rid] = sweepResult{uuid: uuid, data: bytes.Clone(data), err: err}
		return nil
	})
	if err != nil {
		t.Fatalf("ExpandAll: %v", err)
	}
	return got
}

// TestExpandAll_MatchesCacheExpand pins that the sweep reports, for every
// non-phantom blob, exactly what a caching Expand of that blob alone reports:
// the same bytes, or an error with the same text. The repository mixes a
// sound chain with a branch off an interior, a misnamed interior, a delta
// grounded in a phantom and a delta cycle.
func TestExpandAll_MatchesCacheExpand(t *testing.T) {
	d := setupTestDB(t)
	rids, _ := buildDeltaChain(t, d, 30)

	branch, _, err := blob.Store(d, []byte(strings.Repeat("line 0010: the quick brown fox jumps over the lazy dog\n", 300)))
	if err != nil {
		t.Fatalf("store branch: %v", err)
	}
	if err := d.WithTx(func(tx *db.Tx) error {
		_, err := Deltify(tx, branch, rids[10])
		return err
	}); err != nil {
		t.Fatalf("deltify branch: %v", err)
	}
	misnamed := rids[5]
	if _, err := d.Exec("UPDATE blob SET uuid=? WHERE rid=?", strings.Repeat("0", 40), misnamed); err != nil {
		t.Fatalf("misname: %v", err)
	}

	phantom, err := blob.StorePhantom(d, strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("store phantom: %v", err)
	}
	var orphaned [3]libfossil.FslID
	for i := range orphaned {
		orphaned[i], _, err = blob.Store(d, []byte(fmt.Sprintf("orphaned %d", i)))
		if err != nil {
			t.Fatalf("store orphaned %d: %v", i, err)
		}
	}
	for _, edge := range [][2]libfossil.FslID{
		{orphaned[0], phantom}, {orphaned[1], orphaned[2]}, {orphaned[2], orphaned[1]},
	} {
		if _, err := d.Exec("INSERT INTO delta(rid, srcid) VALUES(?, ?)", edge[0], edge[1]); err != nil {
			t.Fatalf("insert delta %v: %v", edge, err)
		}
	}

	got := collectSweep(t, d)

	rows, err := d.Query("SELECT rid, uuid FROM blob WHERE size >= 0")
	if err != nil {
		t.Fatalf("query blobs: %v", err)
	}
	defer rows.Close()
	var checked, failed int
	for rows.Next() {
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		checked++
		g, ok := got[rid]
		if !ok {
			t.Fatalf("rid %d not visited", rid)
		}
		if g.uuid != uuid {
			t.Errorf("rid %d: uuid %q, want %q", rid, g.uuid, uuid)
		}
		want, wantErr := NewCache(64<<20).Expand(d, rid)
		if fmt.Sprint(g.err) != fmt.Sprint(wantErr) {
			t.Errorf("rid %d: err %v, want %v", rid, g.err, wantErr)
		}
		if wantErr != nil {
			failed++
		}
		if !bytes.Equal(g.data, want) {
			t.Errorf("rid %d: content differs from Expand", rid)
		}
	}
	if len(got) != checked {
		t.Fatalf("visited %d blobs, want %d non-phantom", len(got), checked)
	}
	// rids[0..5] sit at or below the misnamed node; the three orphans never
	// reach a real root.
	if failed != 6+len(orphaned) {
		t.Fatalf("failed = %d, want %d", failed, 6+len(orphaned))
	}
}

// TestExpandAll_ReadsEachBlobOnce is the regression guard against quadratic
// expansion: however deep a delta chain is, a sweep reads each stored blob
// exactly once and never walks a chain row by row.
func TestExpandAll_ReadsEachBlobOnce(t *testing.T) {
	const chainLen = 300 // buildDeltaChain's bodies stay distinct up to 300

	d := setupTestDB(t)
	rids, bodies := buildDeltaChain(t, d, chainLen)
	want := make(map[libfossil.FslID][]byte, chainLen)
	for i, rid := range rids {
		want[rid] = bodies[i]
	}

	q := &countingQuerier{inner: d}
	got := collectSweep(t, q)
	for rid, body := range want {
		if g := got[rid]; g.err != nil || !bytes.Equal(g.data, body) {
			t.Fatalf("rid %d: err=%v, content match=%v", rid, g.err, bytes.Equal(g.data, body))
		}
	}
	if q.blobLoads != chainLen {
		t.Fatalf("blob content reads = %d, want %d (one per blob)", q.blobLoads, chainLen)
	}
	if q.deltaWalks != 0 {
		t.Fatalf("per-row delta walks = %d, want 0", q.deltaWalks)
	}
}
