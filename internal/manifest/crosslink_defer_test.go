package manifest

import (
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/repo"
)

// TestCrosslink_DefersCheckinWithMissingFileBlob reproduces the multi-blob
// delivery race surfaced by agent-infra trial #10 finding #12.
//
// Mechanism: a leaf Pulls a multi-blob session in which the Checkin
// manifest blob lands BEFORE its referenced file blob. v0.4.1's Crosslink
// would have written event/leaf rows for the manifest while
// insertCheckinMlinks silently skipped the missing-blob F-card; a
// downstream Checkout.Update walking the manifest's F-cards via
// expandUUID then hit `blob not found for uuid <hex>`.
//
// The fix defers crosslinking the manifest until the referenced blob is
// also stored locally. The manifest blob remains durable in `blob`; only
// the relational rows are delayed. The next Crosslink sweep — which runs
// on every HandleSync round that received files — picks it up because
// the candidate query selects rids without an event row.
func TestCrosslink_DefersCheckinWithMissingFileBlob(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("file content arrives later")
	fileUUID := hash.SHA1(fileContent)

	// Build a Checkin manifest that references fileUUID, but do NOT
	// store the file blob yet. Mirrors the receiver state at the
	// instant a manifest blob is stored before its file blob arrives.
	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "merge across forks",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "race/file.txt", UUID: fileUUID}},
		U:    "tester",
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}
	rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
		if uuid == fileUUID {
			return fileContent, nil
		}
		return nil, fmt.Errorf("unknown uuid: %s", uuid)
	})
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	d.R = rHash
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	manifestRID, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("blob.Store(manifest): %v", err)
	}

	// First Crosslink sweep: the file blob is missing, so the manifest
	// must be deferred. No event/leaf/mlink rows must be written.
	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink (deferred phase): %v", err)
	}
	if linked != 0 {
		t.Errorf("Crosslink (deferred phase): linked = %d, want 0", linked)
	}
	assertCounts(t, r, manifestRID, 0, 0, 0, "after defer")

	// Now the missing file blob arrives. The next Crosslink sweep
	// re-discovers the manifest (no event row was written) and links
	// it fully.
	if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
		t.Fatalf("blob.Store(file): %v", err)
	}
	linked, err = Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink (post-arrival phase): %v", err)
	}
	if linked != 1 {
		t.Errorf("Crosslink (post-arrival phase): linked = %d, want 1", linked)
	}
	assertCounts(t, r, manifestRID, 1, 1, 1, "after blob arrival")
}

// TestCrosslink_DefersDeltaCheckinWithMissingBaseline covers the B-card
// path: a delta manifest references a baseline manifest UUID. If the
// baseline blob isn't local yet, ListFiles cannot resolve the effective
// F-card set, and crosslink must defer.
func TestCrosslink_DefersDeltaCheckinWithMissingBaseline(t *testing.T) {
	r := setupTestRepo(t)

	// Build a "remote" baseline manifest by hand (we never store its blob).
	baselineFileContent := []byte("baseline file")
	baselineFileUUID := hash.SHA1(baselineFileContent)
	baseline := &deck.Deck{
		Type: deck.Checkin,
		C:    "baseline",
		D:    time.Date(2026, 4, 26, 11, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "f.txt", UUID: baselineFileUUID}},
		U:    "tester",
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}
	bR, err := baseline.ComputeR(func(uuid string) ([]byte, error) {
		if uuid == baselineFileUUID {
			return baselineFileContent, nil
		}
		return nil, fmt.Errorf("unknown uuid: %s", uuid)
	})
	if err != nil {
		t.Fatalf("ComputeR baseline: %v", err)
	}
	baseline.R = bR
	baselineBytes, err := baseline.Marshal()
	if err != nil {
		t.Fatalf("Marshal baseline: %v", err)
	}
	baselineUUID := hash.SHA1(baselineBytes)

	// Delta manifest pointing at the baseline (B-card) — no file
	// changes, but the baseline isn't local.
	delta := &deck.Deck{
		Type: deck.Checkin,
		B:    baselineUUID,
		C:    "delta atop missing baseline",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		U:    "tester",
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}
	deltaR, err := delta.ComputeR(func(uuid string) ([]byte, error) {
		// Delta manifest: ComputeR walks baseline files. Provide them.
		if uuid == baselineFileUUID {
			return baselineFileContent, nil
		}
		return nil, fmt.Errorf("unknown uuid: %s", uuid)
	})
	if err != nil {
		t.Fatalf("ComputeR delta: %v", err)
	}
	delta.R = deltaR
	deltaBytes, err := delta.Marshal()
	if err != nil {
		t.Fatalf("Marshal delta: %v", err)
	}
	deltaRID, _, err := blob.Store(r.DB(), deltaBytes)
	if err != nil {
		t.Fatalf("blob.Store(delta): %v", err)
	}

	// Without baseline, Crosslink must defer.
	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink (deferred): %v", err)
	}
	if linked != 0 {
		t.Errorf("linked = %d, want 0 (delta baseline missing)", linked)
	}
	assertCounts(t, r, deltaRID, 0, 0, 0, "delta deferred")

	// Baseline arrives (and its file). Crosslink should link both.
	if _, _, err := blob.Store(r.DB(), baselineFileContent); err != nil {
		t.Fatalf("blob.Store(baseline file): %v", err)
	}
	if _, _, err := blob.Store(r.DB(), baselineBytes); err != nil {
		t.Fatalf("blob.Store(baseline manifest): %v", err)
	}
	linked, err = Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink (post-baseline): %v", err)
	}
	// Both the baseline manifest and the delta manifest should be
	// linked in the same sweep.
	if linked != 2 {
		t.Errorf("linked = %d, want 2 (baseline + delta)", linked)
	}
	assertCounts(t, r, deltaRID, 1, 1, 0, "delta linked (no F-cards in delta)")
}

// TestCrosslink_LinksWhenAllBlobsPresent confirms the unchanged path
// continues to link in a single sweep when every referenced blob is
// already stored before Crosslink runs.
func TestCrosslink_LinksWhenAllBlobsPresent(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("present from the start")
	fileUUID := hash.SHA1(fileContent)
	if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
		t.Fatalf("blob.Store(file): %v", err)
	}

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "synchronous",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "syn.txt", UUID: fileUUID}},
		U:    "tester",
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}
	rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
		if uuid == fileUUID {
			return fileContent, nil
		}
		return nil, fmt.Errorf("unknown uuid: %s", uuid)
	})
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	d.R = rHash
	manifestBytes, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	manifestRID, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("blob.Store(manifest): %v", err)
	}

	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked != 1 {
		t.Errorf("linked = %d, want 1", linked)
	}
	assertCounts(t, r, manifestRID, 1, 1, 1, "synchronous link")
}

// TestCrosslink_DefersMultipleCheckinsSharingMissingBlob covers the
// many-to-one case: two distinct Checkin manifests both reference the
// same not-yet-arrived file blob. The first sweep must defer both
// manifests and write zero relational rows; arrival of the single shared
// blob must allow the next sweep to link both manifests.
//
// Methodology: stage two Checkin manifests pointing at the same fileUUID
// (same content -> same hash). Run Crosslink with the file blob absent;
// assert linked == 0. Store the file blob, run Crosslink again; assert
// linked == 2 and both manifests now have event/leaf rows.
func TestCrosslink_DefersMultipleCheckinsSharingMissingBlob(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("shared file across two checkins")
	fileUUID := hash.SHA1(fileContent)

	mkManifest := func(comment string, when time.Time) (int64, *deck.Deck) {
		d := &deck.Deck{
			Type: deck.Checkin,
			C:    comment,
			D:    when,
			F:    []deck.FileCard{{Name: "shared.txt", UUID: fileUUID}},
			U:    "tester",
			T: []deck.TagCard{
				{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
				{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
			},
		}
		rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
			if uuid == fileUUID {
				return fileContent, nil
			}
			return nil, fmt.Errorf("unknown uuid: %s", uuid)
		})
		if err != nil {
			t.Fatalf("ComputeR(%s): %v", comment, err)
		}
		d.R = rHash
		mb, err := d.Marshal()
		if err != nil {
			t.Fatalf("Marshal(%s): %v", comment, err)
		}
		rid, _, err := blob.Store(r.DB(), mb)
		if err != nil {
			t.Fatalf("blob.Store(%s): %v", comment, err)
		}
		return int64(rid), d
	}

	rid1, _ := mkManifest("first", time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC))
	rid2, _ := mkManifest("second", time.Date(2026, 4, 26, 11, 0, 0, 0, time.UTC))

	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink (deferred): %v", err)
	}
	if linked != 0 {
		t.Errorf("linked = %d, want 0 (both deferred)", linked)
	}
	assertCounts(t, r, rid1, 0, 0, 0, "rid1 deferred")
	assertCounts(t, r, rid2, 0, 0, 0, "rid2 deferred")

	if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
		t.Fatalf("blob.Store(file): %v", err)
	}
	linked, err = Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink (post-arrival): %v", err)
	}
	if linked != 2 {
		t.Errorf("linked = %d, want 2 (both manifests link)", linked)
	}
	// Both manifests are sibling leaves (neither has a P-card pointing
	// at the other), so both stay in the leaf table.
	assertCounts(t, r, rid1, 1, 1, 1, "rid1 linked (sibling leaf)")
	assertCounts(t, r, rid2, 1, 1, 1, "rid2 linked (sibling leaf)")
}

// TestCrosslink_DoesNotDeferOnMissingParent confirms a deliberate
// non-deferral: P-card targets that aren't local must NOT block crosslink.
// crosslinkCheckin's parent-resolution helpers (insertCheckinPlinks,
// updateLeafTable) already tolerate missing parents (they skip the row);
// blocking on missing parents would deadlock long-running cherrypick or
// merge histories where ancestors are intentionally pruned.
//
// Methodology: build a Checkin manifest with a P-card pointing at a
// fabricated, never-stored UUID. All F-card blobs are present, so the
// only "missing" reference is the parent. Crosslink must link the
// manifest in a single sweep.
func TestCrosslink_DoesNotDeferOnMissingParent(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("present file")
	fileUUID := hash.SHA1(fileContent)
	if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
		t.Fatalf("blob.Store(file): %v", err)
	}

	// Fabricate a parent UUID that is never stored locally — mirrors a
	// pruned ancestor in a partial clone.
	missingParent := hash.SHA1([]byte("not-a-real-parent"))

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "checkin atop pruned parent",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		P:    []string{missingParent},
		F:    []deck.FileCard{{Name: "p.txt", UUID: fileUUID}},
		U:    "tester",
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}
	rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
		if uuid == fileUUID {
			return fileContent, nil
		}
		return nil, fmt.Errorf("unknown uuid: %s", uuid)
	})
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	d.R = rHash
	mb, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	manifestRID, _, err := blob.Store(r.DB(), mb)
	if err != nil {
		t.Fatalf("blob.Store(manifest): %v", err)
	}

	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked != 1 {
		t.Errorf("linked = %d, want 1 (missing parent must not block)", linked)
	}
	assertCounts(t, r, manifestRID, 1, 1, 1, "linked despite missing parent")
}

// TestCrosslink_DedupesDuplicateFCardUUIDs covers a hostile-input case:
// a single manifest references the same file UUID via multiple F-cards
// (e.g., a renamed file pair where both names point at the same content
// blob — legal in the manifest grammar). The deferral check must visit
// the underlying blob once, not N times, and the missing-list reported
// to slog must contain that UUID once.
//
// Methodology: build a manifest with two F-cards (different names, same
// UUID) pointing at a not-yet-stored blob. Trigger missingCheckinRefs
// indirectly via Crosslink — assert the manifest is deferred (proves the
// missing-blob check ran) and that it links cleanly when the blob lands.
func TestCrosslink_DedupesDuplicateFCardUUIDs(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("dedup target")
	fileUUID := hash.SHA1(fileContent)

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "two F-cards same UUID",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		F: []deck.FileCard{
			{Name: "alpha.txt", UUID: fileUUID},
			{Name: "beta.txt", UUID: fileUUID},
		},
		U: "tester",
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}
	rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
		if uuid == fileUUID {
			return fileContent, nil
		}
		return nil, fmt.Errorf("unknown uuid: %s", uuid)
	})
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	d.R = rHash
	mb, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	manifestRID, _, err := blob.Store(r.DB(), mb)
	if err != nil {
		t.Fatalf("blob.Store(manifest): %v", err)
	}

	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink (defer): %v", err)
	}
	if linked != 0 {
		t.Errorf("linked = %d, want 0 (deferred for missing UUID)", linked)
	}
	assertCounts(t, r, manifestRID, 0, 0, 0, "deferred")

	if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
		t.Fatalf("blob.Store(file): %v", err)
	}
	linked, err = Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink (post-arrival): %v", err)
	}
	if linked != 1 {
		t.Errorf("linked = %d, want 1 after blob arrival", linked)
	}
	// Two F-cards same UUID -> two mlink rows (one per filename).
	assertCounts(t, r, manifestRID, 1, 1, 2, "linked, two mlinks")
}

// assertCounts verifies the relational rows (event/leaf/mlink) for a
// given manifest rid match the expected values. Used to confirm a
// deferred manifest writes nothing, and a fully-linked manifest writes
// exactly the expected rows.
func assertCounts(t *testing.T, r *repo.Repo, manifestRID interface{}, wantEvent, wantLeaf, wantMlink int, label string) {
	t.Helper()
	var ev, lf, ml int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE objid=?", manifestRID).Scan(&ev); err != nil {
		t.Fatalf("%s: count event: %v", label, err)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM leaf WHERE rid=?", manifestRID).Scan(&lf); err != nil {
		t.Fatalf("%s: count leaf: %v", label, err)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM mlink WHERE mid=?", manifestRID).Scan(&ml); err != nil {
		t.Fatalf("%s: count mlink: %v", label, err)
	}
	if ev != wantEvent {
		t.Errorf("%s: event count = %d, want %d", label, ev, wantEvent)
	}
	if lf != wantLeaf {
		t.Errorf("%s: leaf count = %d, want %d", label, lf, wantLeaf)
	}
	if ml != wantMlink {
		t.Errorf("%s: mlink count = %d, want %d", label, ml, wantMlink)
	}
}
