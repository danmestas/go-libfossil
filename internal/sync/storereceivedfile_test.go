package sync

import (
	"testing"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/delta"
	"github.com/danmestas/libfossil/internal/hash"
	_ "github.com/danmestas/libfossil/internal/testdriver"
)

// TestStoreReceivedFileDeltaBeforeBase is the decisive storage-layer test
// for the delta-before-base fix (see issue #53). It constructs the
// ordering explicitly, rather than relying on a mock transport's round
// scheduling to happen to produce it: the delta card for the target is
// stored before the base's content has ever reached the repo.
//
// Before the fix, storeReceivedFile discarded the delta bytes and
// phantomized the *target*. After the fix it must persist the delta,
// linked to a phantomized *base*, and return success — a delta arriving
// before its base is Fossil's normal steady state during a transfer, not
// an error (src/content.c:content_put_ex).
func TestStoreReceivedFileDeltaBeforeBase(t *testing.T) {
	r := setupSyncTestRepo(t)

	base := []byte("original file content, long enough to compress reasonably well")
	target := []byte("original file content, long enough to compress reasonably well, v2")
	deltaBytes := delta.Create(base, target)
	baseUUID := hash.SHA1(base)
	targetUUID := hash.SHA1(target)

	// The delta arrives first. baseUUID has never been seen before.
	if err := storeReceivedFile(r, targetUUID, baseUUID, deltaBytes, nil); err != nil {
		t.Fatalf("storeReceivedFile(delta before base) = %v, want nil: a delta "+
			"arriving before its base must not be treated as an error", err)
	}

	// The target must be a real row, not phantomized — this is the
	// inversion the fix corrects: canonical Fossil phantomizes the base,
	// never the target.
	targetRid, ok := blob.Exists(r.DB(), targetUUID)
	if !ok {
		t.Fatal("target blob row missing after storing delta-before-base")
	}
	var targetSize int64
	if err := r.DB().QueryRow("SELECT size FROM blob WHERE rid=?", targetRid).Scan(&targetSize); err != nil {
		t.Fatalf("query target size: %v", err)
	}
	if targetSize < 0 {
		t.Fatalf("target size = %d, want >= 0 (target must not be phantomized; "+
			"a -1 here means the delta bytes were discarded, the old bug)", targetSize)
	}

	// The target is stored but not yet resolvable, because its base isn't.
	if _, available := content.AvailableByUUID(r.DB(), targetUUID); available {
		t.Fatal("target reported available before its base ever arrived")
	}

	// The base is what got phantomized — a create-phantom-if-missing
	// lookup, mirroring Fossil's rid_from_uuid(&src, 1, ...) in xfer.c.
	baseRid, ok := blob.Exists(r.DB(), baseUUID)
	if !ok {
		t.Fatal("base blob row missing: create-phantom-if-missing lookup did not run")
	}
	var baseSize int64
	if err := r.DB().QueryRow("SELECT size FROM blob WHERE rid=?", baseRid).Scan(&baseSize); err != nil {
		t.Fatalf("query base size: %v", err)
	}
	if baseSize != -1 {
		t.Fatalf("base size = %d, want -1 (phantom, awaiting delivery)", baseSize)
	}

	// The delta-to-source link exists immediately.
	var srcid int64
	if err := r.DB().QueryRow("SELECT srcid FROM delta WHERE rid=?", targetRid).Scan(&srcid); err != nil {
		t.Fatalf("delta row missing for target rid=%d: %v", targetRid, err)
	}
	if srcid != int64(baseRid) {
		t.Fatalf("delta.srcid = %d, want %d", srcid, baseRid)
	}

	// Now the base arrives as ordinary full content, in a later round.
	if err := storeReceivedFile(r, baseUUID, "", base, nil); err != nil {
		t.Fatalf("storeReceivedFile(base) = %v", err)
	}

	// The target resolves lazily now that its base is available — no
	// further write to the target's own row was needed.
	rid, available := content.AvailableByUUID(r.DB(), targetUUID)
	if !available {
		t.Fatal("target still unavailable after its base arrived")
	}
	got, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatalf("content.Expand: %v", err)
	}
	if string(got) != string(target) {
		t.Fatalf("expanded content = %q, want %q", got, target)
	}
}

// TestStoreReceivedFileDeltaChainBeforeAnyBase constructs a two-hop delta
// chain (target -> mid -> base) and delivers only the target's delta in
// the first call, then the mid's delta, then finally the root base — the
// worst-case ordering that used to require one round per chain hop
// (see issue #53's round/file-amplification numbers). Each store call
// must succeed immediately; availability should only resolve once the
// whole chain is grounded.
func TestStoreReceivedFileDeltaChainBeforeAnyBase(t *testing.T) {
	r := setupSyncTestRepo(t)

	base := []byte("root content for chain test, long enough to compress")
	mid := []byte("root content for chain test, long enough to compress, hop one")
	target := []byte("root content for chain test, long enough to compress, hop one, hop two")

	baseUUID := hash.SHA1(base)
	midUUID := hash.SHA1(mid)
	targetUUID := hash.SHA1(target)

	midDelta := delta.Create(base, mid)
	targetDelta := delta.Create(mid, target)

	// Deliver deepest-first: target's delta references mid, which itself
	// doesn't exist yet either.
	if err := storeReceivedFile(r, targetUUID, midUUID, targetDelta, nil); err != nil {
		t.Fatalf("storeReceivedFile(target delta) = %v, want nil", err)
	}
	if err := storeReceivedFile(r, midUUID, baseUUID, midDelta, nil); err != nil {
		t.Fatalf("storeReceivedFile(mid delta) = %v, want nil", err)
	}

	if _, available := content.AvailableByUUID(r.DB(), targetUUID); available {
		t.Fatal("target reported available before the chain is grounded")
	}

	// Finally the root arrives.
	if err := storeReceivedFile(r, baseUUID, "", base, nil); err != nil {
		t.Fatalf("storeReceivedFile(base) = %v", err)
	}

	rid, available := content.AvailableByUUID(r.DB(), targetUUID)
	if !available {
		t.Fatal("target still unavailable after the full chain arrived")
	}
	got, err := content.Expand(r.DB(), rid)
	if err != nil {
		t.Fatalf("content.Expand: %v", err)
	}
	if string(got) != string(target) {
		t.Fatalf("expanded content = %q, want %q", got, target)
	}
}
