package blob

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

func setupTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.fossil")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.CreateRepoSchema(d); err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestStoreAndLoad(t *testing.T) {
	d := setupTestDB(t)
	content := []byte("hello fossil world")

	rid, uuid, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if rid <= 0 {
		t.Fatalf("rid = %d, want > 0", rid)
	}
	if len(uuid) != 40 && len(uuid) != 64 {
		t.Fatalf("uuid length = %d, want 40 or 64", len(uuid))
	}

	got, err := Load(d, rid)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("Load = %q, want %q", got, content)
	}
}

func TestExists(t *testing.T) {
	d := setupTestDB(t)
	content := []byte("existence test")

	_, uuid, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	rid, ok := Exists(d, uuid)
	if !ok {
		t.Fatal("Exists returned false for stored blob")
	}
	if rid <= 0 {
		t.Fatalf("Exists rid = %d, want > 0", rid)
	}

	_, ok = Exists(d, "0000000000000000000000000000000000000000")
	if ok {
		t.Fatal("Exists returned true for non-existent blob")
	}
}

func TestStorePhantom(t *testing.T) {
	d := setupTestDB(t)
	uuid := "da39a3ee5e6b4b0d3255bfef95601890afd80709"

	rid, err := StorePhantom(d, uuid)
	if err != nil {
		t.Fatalf("StorePhantom: %v", err)
	}
	if rid <= 0 {
		t.Fatalf("phantom rid = %d, want > 0", rid)
	}

	var count int
	d.QueryRow("SELECT count(*) FROM phantom WHERE rid=?", rid).Scan(&count)
	if count != 1 {
		t.Fatalf("phantom table count = %d, want 1", count)
	}

	var size int64
	d.QueryRow("SELECT size FROM blob WHERE rid=?", rid).Scan(&size)
	if size != -1 {
		t.Fatalf("phantom size = %d, want -1", size)
	}
}

func TestStoreDelta(t *testing.T) {
	d := setupTestDB(t)
	source := []byte("original content here")
	target := []byte("original content modified")

	srcRid, _, err := Store(d, source)
	if err != nil {
		t.Fatalf("Store source: %v", err)
	}

	tgtRid, _, err := StoreDelta(d, target, srcRid)
	if err != nil {
		t.Fatalf("StoreDelta: %v", err)
	}
	if tgtRid <= 0 {
		t.Fatalf("tgtRid = %d, want > 0", tgtRid)
	}

	var srcid int64
	err = d.QueryRow("SELECT srcid FROM delta WHERE rid=?", tgtRid).Scan(&srcid)
	if err != nil {
		t.Fatalf("delta row missing: %v", err)
	}
	if srcid != int64(srcRid) {
		t.Fatalf("delta.srcid = %d, want %d", srcid, srcRid)
	}
}

func TestStoreMarksUnclustered(t *testing.T) {
	d := setupTestDB(t)
	content := []byte("unclustered test blob")

	rid, _, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	var count int
	d.QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid).Scan(&count)
	if count != 1 {
		t.Fatalf("unclustered count = %d, want 1", count)
	}
}

func TestStoreDeltaMarksUnclustered(t *testing.T) {
	d := setupTestDB(t)
	source := []byte("delta source content here")
	target := []byte("delta target content here")

	srcRid, _, _ := Store(d, source)
	tgtRid, _, err := StoreDelta(d, target, srcRid)
	if err != nil {
		t.Fatalf("StoreDelta: %v", err)
	}

	for _, rid := range []int64{int64(srcRid), int64(tgtRid)} {
		var count int
		d.QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid).Scan(&count)
		if count != 1 {
			t.Fatalf("unclustered count for rid %d = %d, want 1", rid, count)
		}
	}
}

// TestStoreDeltaRawAgainstPhantomBase is the storage-layer test for the
// core of the delta-before-base fix: a delta whose base has not arrived
// yet must be persisted immediately, linked to a phantomized base — not
// discarded, and not stored by phantomizing the target instead (the
// inversion the bug used to make). This constructs the delta-before-base
// ordering explicitly: the base is phantomized (never given real content)
// before the delta referencing it is ever stored.
func TestStoreDeltaRawAgainstPhantomBase(t *testing.T) {
	d := setupTestDB(t)

	source := []byte("original content here, long enough to compress well")
	target := []byte("original content here, long enough to compress well, and modified")
	deltaBytes := delta.Create(source, target)
	targetUUID := hash.SHA1(target)
	baseUUID := hash.SHA1(source)

	// Base arrives nowhere in this test — only its UUID is known, via the
	// delta card that references it. Resolve-or-create-phantom, mirroring
	// Fossil's rid_from_uuid(&src, 1, ...) in src/xfer.c.
	srcRid, err := StorePhantom(d, baseUUID)
	if err != nil {
		t.Fatalf("StorePhantom base: %v", err)
	}

	rid, err := StoreDeltaRaw(d, targetUUID, deltaBytes, srcRid)
	if err != nil {
		t.Fatalf("StoreDeltaRaw: %v", err)
	}
	if rid <= 0 {
		t.Fatalf("rid = %d, want > 0", rid)
	}

	// The target must be a real row, not a phantom: the delta bytes were
	// persisted, not discarded. A size of -1 here would mean we fell back
	// to the old (buggy) behavior of phantomizing the target instead.
	var size int64
	if err := d.QueryRow("SELECT size FROM blob WHERE rid=?", rid).Scan(&size); err != nil {
		t.Fatalf("query target size: %v", err)
	}
	if size != int64(len(target)) {
		t.Fatalf("target size = %d, want %d (target must not be phantomized)", size, len(target))
	}
	var targetPhantomCount int
	d.QueryRow("SELECT count(*) FROM phantom WHERE rid=?", rid).Scan(&targetPhantomCount)
	if targetPhantomCount != 0 {
		t.Fatalf("target rid %d found in phantom table, want absent", rid)
	}

	// The delta-to-source link must exist immediately, even though the
	// source is still unresolved.
	var srcid int64
	if err := d.QueryRow("SELECT srcid FROM delta WHERE rid=?", rid).Scan(&srcid); err != nil {
		t.Fatalf("delta row missing for rid=%d: %v", rid, err)
	}
	if srcid != int64(srcRid) {
		t.Fatalf("delta.srcid = %d, want %d", srcid, srcRid)
	}

	// The base is the one left phantomized — the direction the fix
	// inverts relative to the old behavior.
	var baseSize int64
	d.QueryRow("SELECT size FROM blob WHERE rid=?", srcRid).Scan(&baseSize)
	if baseSize != -1 {
		t.Fatalf("base size = %d, want -1 (base must remain phantom until it arrives)", baseSize)
	}
}

// TestStoreDeltaRawExistingRealBlobIsNoOp mirrors StoreDelta/Store's
// idempotency: if a real (non-phantom) blob already exists for uuid,
// StoreDeltaRaw must not touch it.
func TestStoreDeltaRawExistingRealBlobIsNoOp(t *testing.T) {
	d := setupTestDB(t)

	source := []byte("source content for no-op test, long enough to compress")
	target := []byte("target content for no-op test, long enough to compress")
	rid, _, err := Store(d, target)
	if err != nil {
		t.Fatalf("Store target: %v", err)
	}
	srcRid, _, err := Store(d, source)
	if err != nil {
		t.Fatalf("Store source: %v", err)
	}

	deltaBytes := delta.Create(source, target)
	targetUUID := hash.SHA1(target)

	gotRid, err := StoreDeltaRaw(d, targetUUID, deltaBytes, srcRid)
	if err != nil {
		t.Fatalf("StoreDeltaRaw: %v", err)
	}
	if gotRid != rid {
		t.Fatalf("rid = %d, want %d (existing real blob)", gotRid, rid)
	}

	var deltaCount int
	d.QueryRow("SELECT count(*) FROM delta WHERE rid=?", rid).Scan(&deltaCount)
	if deltaCount != 0 {
		t.Fatalf("delta row count = %d, want 0 (already-real blob must not gain a delta link)", deltaCount)
	}
}

// TestStoreDeltaRawOverflowHeaderRejected is the storage-layer counterpart
// to delta.TestOutputSize_OverflowRejected: the corruption this guards
// against lives in the DB row, not in a parser's return value, so this
// asserts against the row directly. A crafted header that overflows the
// integer parse must not reach the blob table at all -- if it did, and
// the overflow wrapped to math.MaxUint64 (int64(-1)), the resulting row
// would report size=-1 despite holding real content: indistinguishable
// from a genuine phantom, and no phantom-table row to make it
// re-requestable. Permanent, silent corruption from a crafted header.
func TestStoreDeltaRawOverflowHeaderRejected(t *testing.T) {
	d := setupTestDB(t)

	baseUUID := "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	srcRid, err := StorePhantom(d, baseUUID)
	if err != nil {
		t.Fatalf("StorePhantom base: %v", err)
	}

	targetUUID := "0000000000000000000000000000000000000001"
	overflowHeader := []byte("~~~~~~~~~~~\n;0,")

	if _, err := StoreDeltaRaw(d, targetUUID, overflowHeader, srcRid); err == nil {
		t.Fatal("StoreDeltaRaw(overflow header) = nil error, want rejection")
	}

	rid, exists := Exists(d, targetUUID)
	if !exists {
		// Never having created a row at all is the correct outcome too.
		return
	}
	var size int64
	if err := d.QueryRow("SELECT size FROM blob WHERE rid=?", rid).Scan(&size); err != nil {
		t.Fatalf("query size: %v", err)
	}
	if size < 0 {
		t.Fatalf("target size = %d, want >= 0 or no row at all -- a rejected store must never "+
			"leave behind a row whose size collides with the phantom sentinel", size)
	}
}

// TestStoreDeltaRawEmptyPayloadReturnsError is a regression test for a
// remote-triggerable panic: a peer sending a delta file card with
// zero-length content must produce an error, not crash the process. This
// asserts the returned error directly rather than relying on a
// recover()-based probe, so a regression back to a panic fails the test
// (and the process) instead of being masked by matching on panic text.
func TestStoreDeltaRawEmptyPayloadReturnsError(t *testing.T) {
	d := setupTestDB(t)

	baseUUID := "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	srcRid, err := StorePhantom(d, baseUUID)
	if err != nil {
		t.Fatalf("StorePhantom base: %v", err)
	}

	targetUUID := "0000000000000000000000000000000000000002"
	_, err = StoreDeltaRaw(d, targetUUID, []byte{}, srcRid)
	if err == nil {
		t.Fatal("StoreDeltaRaw(empty payload) = nil error, want an error " +
			"(wire-supplied empty content must not panic)")
	}
}

func TestStoreExistingBlobSkipsUnclustered(t *testing.T) {
	d := setupTestDB(t)
	content := []byte("idempotent blob test")

	rid, _, _ := Store(d, content)
	d.Exec("DELETE FROM unclustered WHERE rid=?", rid)

	rid2, _, err := Store(d, content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if rid2 != rid {
		t.Fatalf("rid = %d, want %d (same blob)", rid2, rid)
	}

	var count int
	d.QueryRow("SELECT count(*) FROM unclustered WHERE rid=?", rid).Scan(&count)
	if count != 0 {
		t.Fatalf("unclustered count = %d, want 0 (already-existing blob)", count)
	}
}

// TestStoreVerifyCatchesBuggify enables BUGGIFY and runs Store in a loop.
// The Decompress BUGGIFY site (2% truncation) should cause at least one
// verify failure, confirming the round-trip check catches corruption.
func TestStoreVerifyCatchesBuggify(t *testing.T) {
	var caught int
	for seed := int64(0); seed < 50; seed++ {
		d := setupTestDB(t)
		simio.EnableBuggify(seed)

		content := []byte(fmt.Sprintf("buggify store test seed=%d with enough data to compress", seed))
		_, _, err := Store(d, content)

		simio.DisableBuggify()

		if err != nil && strings.Contains(err.Error(), "verify") {
			caught++
		}
	}
	if caught == 0 {
		t.Fatal("expected at least one verify failure across 50 BUGGIFY seeds")
	}
	t.Logf("caught %d verify failures out of 50 seeds", caught)
}

// TestStoreDeltaVerifyCatchesBuggify enables BUGGIFY and runs StoreDelta in a loop.
// The Decompress BUGGIFY site should cause verify failures on delta round-trip.
func TestStoreDeltaVerifyCatchesBuggify(t *testing.T) {
	var caught int
	// StoreDelta calls Decompress multiple times (source load + verify read-back),
	// so we need more seeds to hit the 2% BUGGIFY on the right call.
	for seed := int64(0); seed < 200; seed++ {
		d := setupTestDB(t)

		// Store source without BUGGIFY so we have a clean base.
		source := []byte("delta source content that is long enough to compress well in the test")
		srcRid, _, err := Store(d, source)
		if err != nil {
			t.Fatalf("seed %d: Store source: %v", seed, err)
		}

		simio.EnableBuggify(seed)

		target := []byte(fmt.Sprintf("delta target content modified for seed=%d with padding to ensure compression", seed))
		_, _, err = StoreDelta(d, target, srcRid)

		simio.DisableBuggify()

		if err != nil && strings.Contains(err.Error(), "verify") {
			caught++
		}
	}
	if caught == 0 {
		t.Fatal("expected at least one verify failure across 200 BUGGIFY seeds")
	}
	t.Logf("caught %d verify failures out of 200 seeds", caught)
}

func BenchmarkStore(b *testing.B) {
	d := func() *db.DB {
		path := filepath.Join(b.TempDir(), "bench.fossil")
		d, _ := db.Open(path)
		db.CreateRepoSchema(d)
		return d
	}()
	defer d.Close()

	data := bytes.Repeat([]byte("benchmark data"), 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		content := append(data, byte(i), byte(i>>8))
		Store(d, content)
	}
}
