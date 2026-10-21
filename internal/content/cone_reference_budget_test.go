package content

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/delta"

	"github.com/danmestas/go-libfossil/internal/blob"
)

func TestConeReferenceBudgetVerifiedDelta(t *testing.T) {
	d := setupTestDB(t)
	base := bytes.Repeat([]byte("0123456789abcdefghijklmnopqrstuv"), 2048)
	target := append([]byte(nil), base...)
	target[len(target)/2] = 'X'
	baseRID, _, err := blob.Store(d, base)
	if err != nil {
		t.Fatal(err)
	}
	targetRID, uuid, err := blob.Store(d, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.WithTx(func(tx *db.Tx) error {
		saved, err := Deltify(tx, targetRID, baseRID)
		if err == nil && saved == 0 {
			t.Fatal("fixture did not store a delta")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	budget := ReferenceBudget{Artifacts: 10, StoredBytes: 1 << 20,
		InflatedBytes: 1 << 20, ExpandedBytes: 1 << 20, Cards: 100}
	facts, work, err := LoadConeReferenceFactsBounded(context.Background(), d, budget)
	if err != nil || facts == nil {
		t.Fatal(err)
	}
	if facts.held[uuid] != targetRID || work.ExpandBytes != int64(len(target)) {
		t.Fatalf("valid delta not hash-verified within budget: %+v", work)
	}
}

func TestConeReferenceBudgetRejectsLyingInflationPrefix(t *testing.T) {
	d := setupTestDB(t)
	rid, _, err := blob.Store(d, bytes.Repeat([]byte{'x'}, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var packed []byte
	if err := d.QueryRow("SELECT content FROM blob WHERE rid=?", rid).Scan(&packed); err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(packed[:4], 1)
	if _, err := d.Exec("UPDATE blob SET content=?,size=1 WHERE rid=?", packed, rid); err != nil {
		t.Fatal(err)
	}
	budget := ReferenceBudget{Artifacts: 10, StoredBytes: 4096, InflatedBytes: 1024, ExpandedBytes: 1024, Cards: 10}
	_, work, err := LoadConeReferenceFactsBounded(context.Background(), d, budget)
	if err == nil || work.InflateBytes != 1 || work.ExpandBytes != 0 || work.Visits != 0 {
		t.Fatalf("lying prefix must not allocate actual expansion: work=%+v err=%v", work, err)
	}
}

func TestConeReferenceBudgetBeforeDeltaAllocation(t *testing.T) {
	d := setupTestDB(t)
	base := []byte("base content")
	baseRID, _, err := blob.Store(d, base)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := delta.Create(base, bytes.Repeat([]byte{'x'}, 4096))
	if err != nil {
		t.Fatal(err)
	}
	rid, _, err := blob.Store(d, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO delta(rid,srcid) VALUES(?,?)", rid, baseRID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("UPDATE blob SET size=1 WHERE rid=?", rid); err != nil {
		t.Fatal(err)
	}
	budget := ReferenceBudget{Artifacts: 10, StoredBytes: 4096, InflatedBytes: 4096, ExpandedBytes: 1024, Cards: 10}
	_, work, err := LoadConeReferenceFactsBounded(context.Background(), d, budget)
	if err == nil || work.ExpandBytes != 0 {
		t.Fatalf("delta allocated before output bound: work=%+v err=%v", work, err)
	}
}

// SQLite permits TEXT in a BLOB-affinity column. length(TEXT) stops at NUL,
// so only a BLOB cast proves the bytes a subsequent payload read allocates.
func TestConeReferenceBudgetCountsStoredBytesNotText(t *testing.T) {
	d := setupTestDB(t)
	rid, _, err := blob.Store(d, []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	packed := append([]byte{1, 1, 1, 1, 0x78, 0x9c, 0}, bytes.Repeat([]byte{'x'}, 1<<20)...)
	if _, err := d.Exec("UPDATE blob SET content=CAST(? AS TEXT),size=1 WHERE rid=?", packed, rid); err != nil {
		t.Fatal(err)
	}
	budget := ReferenceBudget{Artifacts: 10, StoredBytes: 4096,
		InflatedBytes: 32 << 20, ExpandedBytes: 4096, Cards: 10}
	_, work, err := LoadConeReferenceFactsBounded(context.Background(), d, budget)
	if err == nil || work.PayloadReads != 0 || work.InflateBytes != 0 {
		t.Fatalf("TEXT bypassed byte preflight: work=%+v err=%v", work, err)
	}
}

func TestConeReferenceBudgetBeforeExpansion(t *testing.T) {
	d := setupTestDB(t)
	if _, _, err := blob.Store(d, []byte("bounded raw facts")); err != nil {
		t.Fatal(err)
	}
	budget := ReferenceBudget{Artifacts: 10, StoredBytes: 1, InflatedBytes: 1024, ExpandedBytes: 1024, Cards: 10}
	_, work, err := LoadConeReferenceFactsBounded(context.Background(), d, budget)
	if err == nil || work.PayloadReads != 0 || work.InflateBytes != 0 || work.Visits != 0 {
		t.Fatalf("preflight must reject before content read/allocation: work=%+v err=%v", work, err)
	}
	budget.StoredBytes = 1024
	facts, work, err := LoadConeReferenceFactsBounded(context.Background(), d, budget)
	if err != nil || facts == nil || work.PayloadReads != 1 || work.Visits != 1 {
		t.Fatalf("valid bounded preparation: work=%+v err=%v", work, err)
	}
}
