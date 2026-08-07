package manifest

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestAfterDephantomizeCascadeDefersCheckinWithMissingFileBlob is the
// cascade's analogue of TestCrosslink_DefersCheckinWithMissingFileBlob
// (issue #180): the mid-round phantom-fill cascade must hold back a Checkin
// manifest whose referenced blob has not arrived, exactly as the
// whole-repository sweep's deferCheckin does, so a later Crosslink sweep --
// triggered once the missing blob arrives -- can still rediscover and
// complete it.
//
// Before the fix, afterDephantomize called linkArtifact for a Checkin
// unconditionally: it wrote the event row even though insertCheckinMlinks
// silently skipped the missing-blob F-card. Because
// collectCrosslinkCandidates only reselects a rid with NO event row, that
// half-linked checkin was permanently stranded -- no future sweep, no matter
// how many rounds ran or when the blob arrived, would ever revisit it. A
// downstream Checkout.Update walking to that checkin would hit "blob not
// found" with no path to recover.
func TestAfterDephantomizeCascadeDefersCheckinWithMissingFileBlob(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("cascade race: file content arrives later")
	fileUUID := repoHash(r, fileContent)

	// A Checkin manifest referencing fileUUID, stored as full (non-delta)
	// content -- what storeResolvedContent produces for a "file" card, and
	// exactly the dephantomizedRid that storeReceivedFile then hands to
	// manifest.AfterDephantomize. The file blob is deliberately never stored.
	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "cascade race with missing file blob",
		U:    deck.User("tester"),
		D:    time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "race/file.txt", UUID: fileUUID}},
		// A root check-in declares its branch -- both fossil's own `init` and
		// this package's Checkin do -- and leaf only ever considers a check-in
		// that carries a branch tag. See repairLeafTable.
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	manifestRID, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("blob.Store(manifest): %v", err)
	}

	// Drive the manifest through the cascade exactly as
	// storeReceivedFile -> manifest.AfterDephantomize does for a just-stored
	// full-content blob. The file blob it references is missing, so this must
	// defer: no event/leaf/mlink rows at all.
	AfterDephantomize(context.Background(), r, manifestRID)
	assertCounts(t, r, manifestRID, 0, 0, 0, "after cascade, blob missing")

	// The missing file blob arrives (a later round). The next Crosslink sweep
	// -- HandleSync's post-round trigger, or sync.Clone's terminal sweep --
	// must rediscover the manifest (no event row exists yet) and link it
	// fully. This half is what proves recovery is actually restored: a defer
	// that never gets picked back up would still pass the assertion above.
	if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
		t.Fatalf("blob.Store(file): %v", err)
	}
	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink (post-arrival phase): %v", err)
	}
	if linked != 1 {
		t.Errorf("Crosslink (post-arrival phase): linked = %d, want 1", linked)
	}
	assertCounts(t, r, manifestRID, 1, 1, 1, "after blob arrival")
}
