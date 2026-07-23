package sync

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
	"github.com/danmestas/go-libfossil/internal/hash"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestStoreReceivedFileEmptyDeltaPayloadReturnsError is the end-to-end
// regression test for the empty-payload panic: storeReceivedFile guarded
// payload == nil, which an empty non-nil slice sails past, and nothing
// between wire decode (xfer card Content) and blob.StoreDeltaRaw checked
// for zero length. A peer sending a delta file card with empty content
// must not crash the process on either the clone/pull receive path or the
// server push path — both call storeReceivedFile.
func TestStoreReceivedFileEmptyDeltaPayloadReturnsError(t *testing.T) {
	r := setupSyncTestRepo(t)

	baseUUID := hash.SHA1([]byte("some base content, unrelated to this test"))
	targetUUID := hash.SHA1([]byte("some target content, unrelated to this test"))

	err := storeReceivedFile(r, targetUUID, baseUUID, []byte{}, nil)
	if err == nil {
		t.Fatal("storeReceivedFile(empty delta payload) = nil error, want an error " +
			"(a peer-supplied empty delta must not panic the process)")
	}
}

// TestStoreReceivedFileNonHexDeltaSrcReturnsError is a regression test for
// a permanent-storage-pollution hole: the blob table's own CHECK
// constraint only bounds uuid length (>=40), not hex-ness, so a non-hex
// string of valid length -- e.g. 40 'z' characters -- passed it and would
// otherwise become a permanent, unfillable blob.StorePhantom row. Worse
// on the pull path specifically: loadDBPhantoms re-requests every DB
// phantom row every sync round forever, so one hostile delta card bought
// perpetual gimme traffic for content that could never arrive. The target
// uuid gets this same check via hash.IsValidHash; deltaSrc needs it too.
func TestStoreReceivedFileNonHexDeltaSrcReturnsError(t *testing.T) {
	r := setupSyncTestRepo(t)

	target := []byte("target content for the non-hex delta source test, padded a bit")
	targetUUID := hash.SHA1(target)
	base := []byte("base content for the non-hex delta source test, padded a bit")
	deltaBytes := delta.Create(base, target)

	nonHexDeltaSrc := ""
	for i := 0; i < 40; i++ {
		nonHexDeltaSrc += "z" // valid length, not a hex digit
	}

	if err := storeReceivedFile(r, targetUUID, nonHexDeltaSrc, deltaBytes, nil); err == nil {
		t.Fatal("storeReceivedFile(non-hex deltaSrc) = nil error, want rejection")
	}

	// No phantom row -- and no target row -- should have been created for
	// the rejected delta source.
	if _, exists := blob.Exists(r.DB(), nonHexDeltaSrc); exists {
		t.Fatal("a blob row was created for a non-hex deltaSrc -- rejection must happen " +
			"before any row is written, not after")
	}
	if _, exists := blob.Exists(r.DB(), targetUUID); exists {
		t.Fatal("a blob row was created for the target despite its delta source being rejected")
	}
}

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

// TestStoreReceivedFileStoresVerbatimBlobForFullContent is the regression
// test for issue #112 on the full-content receive path: when a caller
// supplies storedBlob (bytes already in Fossil's on-disk blob format, as
// xfer.CFileCard.StoredBlob carries them), storeReceivedFile must persist
// those bytes unchanged rather than recompressing fullContent with its own
// zlib writer -- otherwise a clone reproduces the source's content but not
// its stored bytes.
func TestStoreReceivedFileStoresVerbatimBlobForFullContent(t *testing.T) {
	r := setupSyncTestRepo(t)

	full := []byte("full content received over the wire, long enough to compress")
	uuid := hash.SHA1(full)

	storedBlob, err := blob.Compress(full)
	if err != nil {
		t.Fatalf("blob.Compress: %v", err)
	}
	// Distinctive marker so the test fails loudly if the verbatim bytes are
	// silently discarded in favor of a fresh compression pass.
	storedBlob = append(append([]byte{}, storedBlob...), []byte("-marker-not-part-of-real-zlib")...)

	if err := storeReceivedFile(r, uuid, "", full, storedBlob); err != nil {
		t.Fatalf("storeReceivedFile: %v", err)
	}

	rid, ok := blob.Exists(r.DB(), uuid)
	if !ok {
		t.Fatal("blob row missing after store")
	}
	var gotContent []byte
	if err := r.DB().QueryRow("SELECT content FROM blob WHERE rid=?", rid).Scan(&gotContent); err != nil {
		t.Fatalf("query content: %v", err)
	}
	if string(gotContent) != string(storedBlob) {
		t.Fatalf("blob.content was recompressed instead of stored verbatim:\n  got  %x\n  want %x",
			gotContent, storedBlob)
	}
}

// TestStoreReceivedFileRejectsContentNotMatchingClaimedUUID is the
// integrity regression test for issue #53's raw-delta path: a hostile
// peer can claim any UUID it likes for a delta card, since the base
// needed to check that claim is exactly the thing the peer controls the
// timing of. It ships a delta that expands to "evil" while claiming the
// UUID of "real" (the good content), withholds the base until after the
// delta is accepted, then delivers the base. Nothing may ever serve
// "evil" under real's UUID.
//
// Four things can make this test pass for the wrong reason; each is
// guarded explicitly:
//
//  1. If delta.Create degenerates to an all-literal encoding instead of a
//     copy+insert delta, the raw-delta path never runs and this becomes a
//     no-op test of the full-content path. Guarded by asserting evilDelta
//     compressed relative to evil.
//  2. If the base were already present when the delta arrives, the eager
//     verify-on-receipt path (storeResolvedContent, reached indirectly)
//     would catch the mismatch immediately, proving nothing about the
//     raw-delta path. Guarded by asserting the base is absent first.
//  3. The first store must succeed (delta-before-base is not an error).
//     Fatal on any error there rather than silently falling through, so a
//     future change moving rejection earlier gets noticed, not masked.
//  4. The final check must be on an error from content.Expand, not merely
//     on the returned bytes differing from "evil" -- a fix that served
//     different-but-still-wrong bytes would pass a got != evil check.
//
// delta.Apply's own trailing checksum (see delta.go) is NOT a defense
// here: delta.Create computed that checksum over "evil", so Apply's
// internal consistency check passes even though the content is wrong.
// Only a hash comparison against the claimed UUID (content.Expand) binds
// content to name.
func TestStoreReceivedFileRejectsContentNotMatchingClaimedUUID(t *testing.T) {
	r := setupSyncTestRepo(t)

	base := []byte("legitimate base content, padded out so a delta is worthwhile")
	real := append(append([]byte{}, base...), []byte(", GOOD")...)
	evil := append(append([]byte{}, base...), []byte(", EVIL")...)
	if len(real) != len(evil) {
		t.Fatalf("fixture bug: real and evil must be the same length (%d vs %d)", len(real), len(evil))
	}

	claimedUUID := hash.SHA1(real) // attacker claims the GOOD uuid
	baseUUID := hash.SHA1(base)
	evilDelta := delta.Create(base, evil) // but ships a delta expanding to EVIL

	// Guard 1: the delta must actually compress relative to the full evil
	// content, or this test exercises the wrong code path.
	if len(evilDelta) >= len(evil) {
		t.Fatalf("fixture bug: evilDelta (%d bytes) did not compress relative to evil (%d bytes) — "+
			"delta.Create degenerated to an all-literal encoding, so this test would exercise the "+
			"full-content path, not the raw-delta path", len(evilDelta), len(evil))
	}

	// Guard 2: the base must be genuinely absent before the first store.
	if _, exists := blob.Exists(r.DB(), baseUUID); exists {
		t.Fatal("fixture bug: base must be absent before the first store, or this test proves " +
			"nothing about the raw-delta path")
	}

	// Guard 3: the delta arrives first, base absent, claiming the GOOD uuid.
	if err := storeReceivedFile(r, claimedUUID, baseUUID, evilDelta, nil); err != nil {
		t.Fatalf("storeReceivedFile(evil delta, base absent) = %v, want nil: a delta arriving "+
			"before its base must be stored, not rejected up front — rejection has to happen "+
			"where the claim can actually be checked", err)
	}

	// The base is delivered afterward, as ordinary full content.
	if err := storeReceivedFile(r, baseUUID, "", base, nil); err != nil {
		t.Fatalf("storeReceivedFile(base) = %v", err)
	}

	rid, available := content.AvailableByUUID(r.DB(), claimedUUID)
	if !available {
		t.Fatal("expected the chain to report available once the base arrived — " +
			"if this changed, the test needs updating, not silently passing")
	}

	// Guard 4: the assertion is that Expand refuses to serve anything, not
	// merely that it didn't return "evil" verbatim.
	got, err := content.Expand(r.DB(), rid)
	if err == nil {
		t.Fatalf("content.Expand served %d bytes under uuid=%s, which hashes to a different "+
			"UUID than claimed — a hostile peer planted content under a name it does not hash to",
			len(got), claimedUUID)
	}
}

// TestStoreReceivedFileFillingPhantomCrosslinksDeltaChild is the end-to-end
// regression test for issue #75: manifest.AfterDephantomize was ported,
// tested in isolation, and never wired into the real fill path. This test
// proves it now fires from storeReceivedFile itself -- not merely from a
// later manifest.Crosslink() sweep -- by checking crosslink side effects
// (the 'event' row a checkin manifest produces) immediately after the base
// arrives, with no intervening call to manifest.Crosslink anywhere in the
// test.
//
// A checkin manifest is delivered first, as a raw delta against a base
// blob that hasn't arrived yet (base becomes a phantom via
// create-phantom-if-missing, mirroring how a real sync round can deliver a
// delta child before its source). Because the base is unavailable, the
// manifest cannot be expanded or crosslinked yet, so no event row exists.
// Once the base's real content arrives and fills the phantom, the fix
// under test crosslinks the manifest immediately as part of storing that
// fill -- proving AfterDephantomize runs on the production fill path, not
// only under go test via its own unit tests.
func TestStoreReceivedFileFillingPhantomCrosslinksDeltaChild(t *testing.T) {
	r := setupSyncTestRepo(t)

	fileContent := []byte("hello dephantomize e2e")
	_, fileUUID, err := blob.Store(r.DB(), fileContent)
	if err != nil {
		t.Fatalf("Store file blob: %v", err)
	}

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "dephantomize e2e commit",
		U:    deck.User("testuser"),
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "hello.txt", UUID: fileUUID}},
	}
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	manifestUUID := hash.SHA1(manifestBytes)

	base := []byte("root content for the dephantomize e2e crosslink test, unrelated to the manifest")
	baseUUID := hash.SHA1(base)
	manifestDelta := delta.Create(base, manifestBytes)

	// The checkin manifest arrives first, as a delta against a base that
	// has never been seen -- create-phantom-if-missing phantomizes the
	// base, mirroring the client.go:storeDeltaContent doc comment.
	if err := storeReceivedFile(r, manifestUUID, baseUUID, manifestDelta, nil); err != nil {
		t.Fatalf("storeReceivedFile(manifest delta, base absent) = %v, want nil", err)
	}

	manifestRid, ok := blob.Exists(r.DB(), manifestUUID)
	if !ok {
		t.Fatal("manifest blob row missing after storing its delta")
	}

	var eventCountBefore int
	r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", manifestRid).Scan(&eventCountBefore)
	if eventCountBefore != 0 {
		t.Fatalf("event count before base arrives = %d, want 0 (manifest not yet expandable)", eventCountBefore)
	}

	// The base arrives as ordinary full content, filling the phantom.
	if err := storeReceivedFile(r, baseUUID, "", base, nil); err != nil {
		t.Fatalf("storeReceivedFile(base) = %v, want nil", err)
	}

	// No manifest.Crosslink() call anywhere above: this must be
	// AfterDephantomize firing on the fill path itself.
	var eventCountAfter int
	var eventType string
	r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", manifestRid).Scan(&eventCountAfter)
	r.DB().QueryRow("SELECT type FROM event WHERE objid=?", manifestRid).Scan(&eventType)
	if eventCountAfter != 1 {
		t.Fatalf("event count after base fills the phantom = %d, want 1 -- "+
			"AfterDephantomize must crosslink delta children of a filled phantom "+
			"as part of storeReceivedFile, not only when go test calls it directly",
			eventCountAfter)
	}
	if eventType != "ci" {
		t.Errorf("event type = %q, want 'ci'", eventType)
	}
}
