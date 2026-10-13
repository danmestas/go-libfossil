package manifest

import (
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestCrosslink_LinksCheckinWithMissingFileBlob is issue #406: a check-in
// whose file has not arrived is linked against a phantom for the file, as
// fossil's add_one_mlink does (uuid_to_rid(..., 1)), rather than held back.
// The hold-back it replaces (agent-infra trial #10 finding #12) had no bound:
// a file that never arrived withheld the check-in's rows forever. When the
// file does arrive it fills the phantom in place, so the mlink row already
// names it and nothing is relinked.
func TestCrosslink_LinksCheckinWithMissingFileBlob(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("file content arrives later")
	fileUUID := repoHash(r, fileContent)

	// Build a Checkin manifest that references fileUUID, but do NOT
	// store the file blob yet. Mirrors the receiver state at the
	// instant a manifest blob is stored before its file blob arrives.
	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "merge across forks",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "race/file.txt", UUID: fileUUID}},
		U:    deck.User("tester"),
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
		t.Errorf("Crosslink: linked = %d, want 1", linked)
	}
	assertCounts(t, r, manifestRID, 1, 1, 1, "linked against a phantom")
	fileRID := assertPhantom(t, r, fileUUID, true)

	// The file arrives: it fills the phantom, and the existing mlink row
	// already names it.
	if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
		t.Fatalf("blob.Store(file): %v", err)
	}
	if linked, err = Crosslink(r); err != nil || linked != 0 {
		t.Fatalf("Crosslink after arrival = %d, %v; want 0", linked, err)
	}
	assertCounts(t, r, manifestRID, 1, 1, 1, "after file arrival")
	if got := assertPhantom(t, r, fileUUID, false); got != fileRID {
		t.Errorf("file rid = %d after arrival, want the phantom's rid %d", got, fileRID)
	}
}

// assertPhantom checks whether uuid is stored as a phantom and returns its
// rid; a uuid with no blob row at all fails the test.
func assertPhantom(t *testing.T, r *repo.Repo, uuid string, want bool) int64 {
	t.Helper()
	var rid, size int64
	if err := r.DB().QueryRow("SELECT rid, size FROM blob WHERE uuid=?", uuid).
		Scan(&rid, &size); err != nil {
		t.Fatalf("blob %s: %v", uuid, err)
	}
	if got := size < 0; got != want {
		t.Errorf("blob %s phantom = %v, want %v", uuid, got, want)
	}
	return rid
}

// TestCrosslink_DefersDeltaCheckinWithMissingBaseline covers the B-card
// path: a delta manifest references a baseline manifest UUID. If the
// baseline blob isn't local yet, ListFiles cannot resolve the effective
// F-card set, and crosslink must defer.
func TestCrosslink_DefersDeltaCheckinWithMissingBaseline(t *testing.T) {
	r := setupTestRepo(t)

	// Build a "remote" baseline manifest by hand (we never store its blob).
	baselineFileContent := []byte("baseline file")
	baselineFileUUID := repoHash(r, baselineFileContent)
	baseline := &deck.Deck{
		Type: deck.Checkin,
		C:    "baseline",
		D:    time.Date(2026, 4, 26, 11, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "f.txt", UUID: baselineFileUUID}},
		U:    deck.User("tester"),
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
	baselineUUID := repoHash(r, baselineBytes)

	// Delta manifest pointing at the baseline (B-card) — no file
	// changes, but the baseline isn't local.
	delta := &deck.Deck{
		Type: deck.Checkin,
		B:    baselineUUID,
		C:    "delta atop missing baseline",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		U:    deck.User("tester"),
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
	// The gap is recorded as fossil's fetch_baseline records it: a phantom for
	// the baseline, so a sync requests it, and an orphan row under it.
	baseRID := assertPhantom(t, r, baselineUUID, true)
	assertOrphan(t, r, int64(deltaRID), baseRID)

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
	var orphans int
	if err := r.DB().QueryRow("SELECT count(*) FROM orphan").Scan(&orphans); err != nil || orphans != 0 {
		t.Errorf("orphan rows after the delta linked = %d, %v; want 0", orphans, err)
	}
}

// TestCrosslink_LinksWhenAllBlobsPresent confirms the unchanged path
// continues to link in a single sweep when every referenced blob is
// already stored before Crosslink runs.
func TestCrosslink_LinksWhenAllBlobsPresent(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("present from the start")
	fileUUID := repoHash(r, fileContent)
	if _, _, err := blob.Store(r.DB(), fileContent); err != nil {
		t.Fatalf("blob.Store(file): %v", err)
	}

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "synchronous",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "syn.txt", UUID: fileUUID}},
		U:    deck.User("tester"),
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

// TestCrosslink_LinksCheckinsSharingMissingBlob: two check-ins naming the
// same absent file both link in one sweep, against one shared phantom.
func TestCrosslink_LinksCheckinsSharingMissingBlob(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("shared file across two checkins")
	fileUUID := repoHash(r, fileContent)

	mkManifest := func(comment string, when time.Time) (int64, *deck.Deck) {
		d := &deck.Deck{
			Type: deck.Checkin,
			C:    comment,
			D:    when,
			F:    []deck.FileCard{{Name: "shared.txt", UUID: fileUUID}},
			U:    deck.User("tester"),
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
		t.Fatalf("Crosslink: %v", err)
	}
	if linked != 2 {
		t.Errorf("linked = %d, want 2 (both manifests link)", linked)
	}
	// Both manifests are sibling leaves (neither has a P-card pointing
	// at the other), so both stay in the leaf table.
	assertCounts(t, r, rid1, 1, 1, 1, "rid1 linked (sibling leaf)")
	assertCounts(t, r, rid2, 1, 1, 1, "rid2 linked (sibling leaf)")
	fileRID := assertPhantom(t, r, fileUUID, true)
	var fids int
	if err := r.DB().QueryRow(
		"SELECT count(*) FROM mlink WHERE mid IN (?, ?) AND fid=?", rid1, rid2, fileRID,
	).Scan(&fids); err != nil || fids != 2 {
		t.Errorf("mlink rows on the shared phantom = %d, %v; want 2", fids, err)
	}
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
	fileUUID := repoHash(r, fileContent)
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
		U:    deck.User("tester"),
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
	// mlink is 0, not 1: an mlink row records a parent-to-child transition, and
	// with the parent's manifest unavailable there is nothing to diff against.
	// Canonical Fossil's add_mlink returns without writing when the parent's
	// content is empty, and derives the rows later from the parent's side once
	// it arrives (issue #193). Claiming every file is an add here is what put
	// 744 bogus rows in a corpus clone.
	assertCounts(t, r, manifestRID, 1, 1, 0, "linked despite missing parent")
}

// TestCrosslink_DoesNotDeferOnMissingFilesAtopMissingParent is issue #398:
// the libfossil repository holds a fossil-scm check-in imported without its
// parent or most of its files. With the primary parent absent no mlink row
// is derived (fossil's add_mlink returns early), so waiting on the files
// could never complete the check-in; the sweep deferred it forever and lost
// the event and plink rows `fossil rebuild` writes. Once the parent arrives,
// its crosslink derives the child's file transition against a phantom for
// the file that is still absent, as fossil's add_one_mlink does.
func TestCrosslink_DoesNotDeferOnMissingFilesAtopMissingParent(t *testing.T) {
	r := setupTestRepo(t)
	parent := &deck.Deck{
		Type: deck.Checkin,
		C:    "parent arrives later",
		D:    time.Date(2026, 4, 26, 11, 0, 0, 0, time.UTC),
		U:    deck.User("tester"),
		T: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
		},
	}
	parentBytes, err := parent.Marshal()
	if err != nil {
		t.Fatalf("Marshal(parent): %v", err)
	}
	child := &deck.Deck{
		Type: deck.Checkin,
		C:    "imported without its history",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		P:    []string{repoHash(r, parentBytes)},
		F:    []deck.FileCard{{Name: "a.txt", UUID: repoHash(r, []byte("never stored"))}},
		U:    deck.User("tester"),
	}
	childBytes, err := child.Marshal()
	if err != nil {
		t.Fatalf("Marshal(child): %v", err)
	}
	childRID, _, err := blob.Store(r.DB(), childBytes)
	if err != nil {
		t.Fatalf("blob.Store(child): %v", err)
	}

	if linked, err := Crosslink(r); err != nil || linked != 1 {
		t.Fatalf("Crosslink = %d, %v; want 1 (absent files must not block)", linked, err)
	}
	// No leaf yet: the child carries no branch tag and has no parent to
	// inherit one from, and fossil never makes an untagged check-in a leaf.
	assertCounts(t, r, childRID, 1, 0, 0, "atop missing parent")
	var plinks int
	if err := r.DB().QueryRow("SELECT count(*) FROM plink WHERE cid=?", childRID).
		Scan(&plinks); err != nil || plinks != 1 {
		t.Fatalf("plink rows = %d, %v; want 1", plinks, err)
	}

	if _, _, err := blob.Store(r.DB(), parentBytes); err != nil {
		t.Fatalf("blob.Store(parent): %v", err)
	}
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink after parent: %v", err)
	}
	assertCounts(t, r, childRID, 1, 1, 1, "after parent arrival")
}

// TestCrosslink_DuplicateFCardUUIDsShareOnePhantom covers a manifest naming
// one absent file blob under two names -- legal in the manifest grammar. It
// links at once with one mlink row per name, both on a single phantom.
func TestCrosslink_DuplicateFCardUUIDsShareOnePhantom(t *testing.T) {
	r := setupTestRepo(t)

	fileContent := []byte("dedup target")
	fileUUID := repoHash(r, fileContent)

	d := &deck.Deck{
		Type: deck.Checkin,
		C:    "two F-cards same UUID",
		D:    time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		F: []deck.FileCard{
			{Name: "alpha.txt", UUID: fileUUID},
			{Name: "beta.txt", UUID: fileUUID},
		},
		U: deck.User("tester"),
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
		t.Errorf("linked = %d, want 1", linked)
	}
	// Two F-cards same UUID -> two mlink rows (one per filename).
	assertCounts(t, r, manifestRID, 1, 1, 2, "linked, two mlinks")
	fileRID := assertPhantom(t, r, fileUUID, true)
	var phantoms int
	if err := r.DB().QueryRow("SELECT count(*) FROM blob WHERE size<0").
		Scan(&phantoms); err != nil || phantoms != 1 {
		t.Errorf("phantoms = %d, %v; want 1", phantoms, err)
	}
	var fids int
	if err := r.DB().QueryRow("SELECT count(*) FROM mlink WHERE mid=? AND fid=?",
		manifestRID, fileRID).Scan(&fids); err != nil || fids != 2 {
		t.Errorf("mlink rows on the phantom = %d, %v; want 2", fids, err)
	}
}

// TestCheckinDeferralGuardBoundsDiagnosticSamples ensures a long receive
// sweep preserves exact diagnostics without retaining every deferred artifact.
// RIDs and missing baselines arrive largest-first, and every second baseline
// is shared; repeated references still count as observations, while each
// bounded sample keeps the smallest distinct identifiers.
func TestCheckinDeferralGuardBoundsDiagnosticSamples(t *testing.T) {
	// Every baseline is shared by two rids, and there are still more distinct
	// baselines than the sample keeps.
	const deferrals = 2*logDeferredSampleSize + 3

	r := setupTestRepo(t)
	guard := newCheckinDeferralGuard()
	baselineOf := func(rid int) string { return fmt.Sprintf("%040x", (rid+1)/2) }

	if err := r.WithTx(func(tx *db.Tx) error {
		for rid := deferrals; rid > 0; rid-- {
			d := &deck.Deck{Type: deck.Checkin, B: baselineOf(rid)}
			deferred, err := guard.shouldDefer(tx, "test", libfossil.FslID(rid), d)
			if err != nil {
				return err
			}
			if !deferred {
				return fmt.Errorf("rid %d was not deferred", rid)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record deferrals: %v", err)
	}

	if got := guard.deferredCount; got != deferrals {
		t.Errorf("deferred event count = %d, want %d", got, deferrals)
	}
	if got, want := guard.missingReferenceCount, deferrals; got != want {
		t.Errorf("missing reference count = %d, want %d", got, want)
	}
	if got := len(guard.deferredRids); got != logDeferredSampleSize {
		t.Errorf("retained deferred RID samples = %d, want %d", got, logDeferredSampleSize)
	}
	if got := len(guard.missingBlobs); got != logDeferredSampleSize {
		t.Errorf("retained missing UUID samples = %d, want %d", got, logDeferredSampleSize)
	}

	retainedRIDs := make(map[libfossil.FslID]struct{}, len(guard.deferredRids))
	for _, rid := range guard.deferredRids {
		retainedRIDs[rid] = struct{}{}
	}
	for rid := 1; rid <= logDeferredSampleSize; rid++ {
		if _, ok := retainedRIDs[libfossil.FslID(rid)]; !ok {
			t.Errorf("missing smallest retained RID %d from %v", rid, guard.deferredRids)
		}
		wantUUID := fmt.Sprintf("%040x", rid)
		if _, ok := guard.missingBlobs[wantUUID]; !ok {
			t.Errorf("missing smallest retained UUID %q from %v", wantUUID, guard.missingBlobs)
		}
	}
}

// assertOrphan checks that rid is filed in orphan under baseline.
func assertOrphan(t *testing.T, r *repo.Repo, rid, baseline int64) {
	t.Helper()
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM orphan WHERE rid=? AND baseline=?",
		rid, baseline).Scan(&n); err != nil || n != 1 {
		t.Errorf("orphan(%d, %d) rows = %d, %v; want 1", rid, baseline, n, err)
	}
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
