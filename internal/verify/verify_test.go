package verify_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/internal/verify"
	"github.com/danmestas/go-libfossil/simio"
)

func newTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	dir := t.TempDir()
	r, err := repo.Create(dir+"/test.fossil", "test", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestVerify_EmptyRepo(t *testing.T) {
	r := newTestRepo(t)
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("expected clean verify on empty repo, got %d issues", len(report.Issues))
	}
	if report.BlobsChecked != 0 {
		t.Fatalf("expected 0 blobs checked on empty repo, got %d", report.BlobsChecked)
	}
}

func TestVerify_CleanRepo(t *testing.T) {
	r := newTestRepo(t)
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "hello.txt", Content: []byte("hello world")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		for _, iss := range report.Issues {
			t.Logf("issue: %s", iss.Message)
		}
		t.Fatalf("expected clean verify, got %d issues", len(report.Issues))
	}
	if report.BlobsChecked == 0 {
		t.Fatal("expected blobs to be checked")
	}
	if report.BlobsOK != report.BlobsChecked {
		t.Fatalf("expected all blobs OK, got %d/%d", report.BlobsOK, report.BlobsChecked)
	}
}

func TestVerify_DetectsHashMismatch(t *testing.T) {
	r := newTestRepo(t)
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("good content")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt a file blob
	_, err = r.DB().Exec(`UPDATE blob SET content = X'0000000000' WHERE rid = (SELECT MAX(rid) FROM blob WHERE size >= 0)`)
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("expected issues after corruption")
	}
	if report.BlobsFailed == 0 {
		t.Fatal("expected at least one failed blob")
	}
	found := false
	for _, iss := range report.Issues {
		if iss.Kind == verify.IssueHashMismatch || iss.Kind == verify.IssueBlobCorrupt {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected IssueHashMismatch or IssueBlobCorrupt")
	}
}

func TestVerify_ReportsAll(t *testing.T) {
	r := newTestRepo(t)
	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("alpha")}},
		Comment: "first",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("alpha")}, {Name: "b.txt", Content: []byte("bravo")}},
		Comment: "second",
		User:    "test",
		Parent:  rid1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt ALL non-phantom blobs
	_, err = r.DB().Exec("UPDATE blob SET content = X'0000' WHERE size >= 0")
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Issues) < 2 {
		t.Fatalf("expected multiple issues (report-all), got %d", len(report.Issues))
	}
}

func TestVerify_DetectsDanglingDelta(t *testing.T) {
	r := newTestRepo(t)
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("content")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Insert a delta row pointing to nonexistent blobs
	_, err = r.DB().Exec("INSERT INTO delta(rid, srcid) VALUES(999999, 888888)")
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iss := range report.Issues {
		if iss.Kind == verify.IssueDeltaDangling {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected IssueDeltaDangling")
	}
}

func TestVerify_DetectsOrphanPhantom(t *testing.T) {
	r := newTestRepo(t)
	// Insert a phantom row with no corresponding blob
	_, err := r.DB().Exec("INSERT INTO phantom(rid) VALUES(999999)")
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iss := range report.Issues {
		if iss.Kind == verify.IssuePhantomOrphan {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected IssuePhantomOrphan")
	}
}

func TestVerify_DetectsMissingEvent(t *testing.T) {
	r := newTestRepo(t)
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("content")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.DB().Exec("DELETE FROM event")
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iss := range report.Issues {
		if iss.Kind == verify.IssueEventMissing {
			found = true
			break
		}
	}
	if !found {
		for _, iss := range report.Issues {
			t.Logf("issue: kind=%d %s", iss.Kind, iss.Message)
		}
		t.Fatal("expected IssueEventMissing")
	}
}

func TestVerify_DetectsMissingPlink(t *testing.T) {
	r := newTestRepo(t)
	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("alpha")}},
		Comment: "first",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("alpha")}, {Name: "b.txt", Content: []byte("bravo")}},
		Comment: "second",
		User:    "test",
		Parent:  rid1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.DB().Exec("DELETE FROM plink")
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iss := range report.Issues {
		if iss.Kind == verify.IssuePlinkMissing {
			found = true
			break
		}
	}
	if !found {
		for _, iss := range report.Issues {
			t.Logf("issue: kind=%d %s", iss.Kind, iss.Message)
		}
		t.Fatal("expected IssuePlinkMissing")
	}
}

func TestRebuild_ReconstructsFromScratch(t *testing.T) {
	r := newTestRepo(t)
	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("alpha")}},
		Comment: "first",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("alpha")}, {Name: "b.txt", Content: []byte("bravo")}},
		Comment: "second",
		User:    "test",
		Parent:  rid1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot counts before
	var eventCount, plinkCount int
	r.DB().QueryRow("SELECT count(*) FROM event").Scan(&eventCount)
	r.DB().QueryRow("SELECT count(*) FROM plink").Scan(&plinkCount)

	// Delete all derived tables
	for _, tbl := range []string{"event", "mlink", "plink", "tagxref", "filename", "leaf", "unclustered", "unsent"} {
		r.DB().Exec("DELETE FROM " + tbl)
	}

	report, err := verify.Rebuild(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.TablesRebuilt) == 0 {
		t.Fatal("expected TablesRebuilt")
	}

	var newEventCount, newPlinkCount int
	r.DB().QueryRow("SELECT count(*) FROM event").Scan(&newEventCount)
	r.DB().QueryRow("SELECT count(*) FROM plink").Scan(&newPlinkCount)
	if newEventCount != eventCount {
		t.Fatalf("event: want %d got %d", eventCount, newEventCount)
	}
	if newPlinkCount != plinkCount {
		t.Fatalf("plink: want %d got %d", plinkCount, newPlinkCount)
	}
}

func TestRebuild_CreatesPhantomForMissingFileBlob(t *testing.T) {
	r := newTestRepo(t)
	const missingUUID = "0123456789abcdef0123456789abcdef01234567"

	manifestBytes, err := (&deck.Deck{
		Type: deck.Checkin,
		C:    "missing file blob",
		D:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		U:    deck.User("test"),
		F: []deck.FileCard{
			{Name: "missing.txt", UUID: missingUUID},
		},
	}).Marshal()
	if err != nil {
		t.Fatalf("marshal checkin manifest: %v", err)
	}
	manifestRID, _, err := blob.Store(r.DB(), manifestBytes)
	if err != nil {
		t.Fatalf("store checkin manifest: %v", err)
	}

	if _, err := verify.Rebuild(r); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	var fid, size int64
	if err := r.DB().QueryRow(`
		SELECT m.fid, b.size
		FROM mlink AS m
		JOIN filename AS f ON f.fnid = m.fnid
		JOIN blob AS b ON b.rid = m.fid
		WHERE m.mid = ? AND f.name = ?
	`, manifestRID, "missing.txt").Scan(&fid, &size); err != nil {
		t.Fatalf("query missing.txt mlink: %v", err)
	}
	if fid <= 0 {
		t.Fatalf("missing.txt fid = %d, want a positive phantom RID", fid)
	}
	if size != -1 {
		t.Fatalf("missing.txt phantom size = %d, want -1", size)
	}

	var deletionRows int
	if err := r.DB().QueryRow(`
		SELECT count(*)
		FROM mlink AS m
		JOIN filename AS f ON f.fnid = m.fnid
		WHERE m.mid = ? AND f.name = ? AND m.fid = 0
	`, manifestRID, "missing.txt").Scan(&deletionRows); err != nil {
		t.Fatalf("count missing.txt deletion mlinks: %v", err)
	}
	if deletionRows != 0 {
		t.Fatalf("missing.txt has %d fid=0 deletion mlinks, want none", deletionRows)
	}
}

func TestRebuild_Idempotent(t *testing.T) {
	r := newTestRepo(t)
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("hello")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	report1, err := verify.Rebuild(r)
	if err != nil {
		t.Fatal(err)
	}
	report2, err := verify.Rebuild(r)
	if err != nil {
		t.Fatal(err)
	}
	if report1.BlobsChecked != report2.BlobsChecked {
		t.Fatalf("blobs: %d vs %d", report1.BlobsChecked, report2.BlobsChecked)
	}
}

func TestRebuild_ReconstructsTags(t *testing.T) {
	r := newTestRepo(t)
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("content")}},
		Comment: "initial on trunk",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	var tagxrefCount int
	r.DB().QueryRow("SELECT count(*) FROM tagxref").Scan(&tagxrefCount)
	if tagxrefCount == 0 {
		t.Fatal("expected tagxref rows after checkin with trunk tags")
	}

	for _, tbl := range []string{"event", "mlink", "plink", "tagxref", "filename", "leaf", "unclustered", "unsent"} {
		r.DB().Exec("DELETE FROM " + tbl)
	}

	report, err := verify.Rebuild(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = report

	var newTagxrefCount int
	r.DB().QueryRow("SELECT count(*) FROM tagxref").Scan(&newTagxrefCount)
	if newTagxrefCount == 0 {
		t.Fatal("expected tagxref rows after rebuild")
	}

	// Verify repo is clean
	vReport, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	if !vReport.OK() {
		for _, iss := range vReport.Issues {
			t.Logf("issue: %s", iss.Message)
		}
		t.Fatalf("expected clean verify after rebuild, got %d issues", len(vReport.Issues))
	}
}

func TestVerify_DetectsIncorrectLeaf(t *testing.T) {
	r := newTestRepo(t)
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("content")}},
		Comment: "initial",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.DB().Exec("DELETE FROM leaf")
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iss := range report.Issues {
		if iss.Kind == verify.IssueLeafIncorrect {
			found = true
			break
		}
	}
	if !found {
		for _, iss := range report.Issues {
			t.Logf("issue: kind=%d %s", iss.Kind, iss.Message)
		}
		t.Fatal("expected IssueLeafIncorrect")
	}
}

func TestRebuild_BuggifyResilience(t *testing.T) {
	r := newTestRepo(t)

	// Create repo BEFORE enabling BUGGIFY
	var files []manifest.File
	for i := 0; i < 20; i++ {
		files = append(files, manifest.File{
			Name:    fmt.Sprintf("file%d.txt", i),
			Content: []byte(fmt.Sprintf("content %d", i)),
		})
	}
	_, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   files,
		Comment: "buggify test",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Enable BUGGIFY after checkin
	simio.EnableBuggify(42)
	defer simio.DisableBuggify()

	report, err := verify.Rebuild(r)
	if err != nil {
		// Rebuild may error if manifest blob is corrupted by BUGGIFY — acceptable
		t.Logf("Rebuild under BUGGIFY returned error (expected): %v", err)
		return
	}

	if len(report.TablesRebuilt) == 0 {
		t.Fatal("expected TablesRebuilt after successful rebuild")
	}
	t.Logf("BUGGIFY rebuild: %d blobs checked, %d failed, %d skipped",
		report.BlobsChecked, report.BlobsFailed, report.BlobsSkipped)
}

func TestVerify_AfterRebuild_IsClean(t *testing.T) {
	r := newTestRepo(t)

	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   []manifest.File{{Name: "a.txt", Content: []byte("alpha")}},
		Comment: "first",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("alpha modified")},
			{Name: "b.txt", Content: []byte("bravo")},
		},
		Comment: "second",
		User:    "test",
		Parent:  rid1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Rebuild from scratch
	_, err = verify.Rebuild(r)
	if err != nil {
		t.Fatal(err)
	}

	// Verify should be completely clean
	report, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		for _, iss := range report.Issues {
			t.Logf("issue: kind=%d table=%s %s", iss.Kind, iss.Table, iss.Message)
		}
		t.Fatalf("expected clean verify after rebuild, got %d issues", len(report.Issues))
	}
}

func TestRebuild_PreservesSparseFileHistory(t *testing.T) {
	r := newTestRepo(t)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("a1")},
			{Name: "b.txt", Content: []byte("b1")},
		},
		Comment: "initial",
		User:    "u",
		Time:    ts,
	})
	if err != nil {
		t.Fatalf("initial checkin: %v", err)
	}
	rid2, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("a2")},
			{Name: "b.txt", Content: []byte("b1")}, // unchanged
		},
		Comment: "modify a",
		User:    "u",
		Time:    ts.Add(time.Hour),
		Parent:  rid1,
	})
	if err != nil {
		t.Fatalf("second checkin: %v", err)
	}
	// Wipe derived tables and rebuild — canonical mlink derivation preserves
	// sparse delta behavior: unchanged files get no mlink row.
	for _, tbl := range []string{"event", "mlink", "plink", "tagxref", "filename", "leaf", "unclustered", "unsent"} {
		r.DB().Exec("DELETE FROM " + tbl)
	}

	report, err := verify.Rebuild(r)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.BlobsFailed > 0 {
		t.Fatalf("Rebuild had %d blob failures", report.BlobsFailed)
	}

	// After rebuild, FileHistory should still work.
	histA, err := manifest.FileHistory(r, manifest.FileHistoryOpts{Path: "a.txt"})
	if err != nil {
		t.Fatalf("FileHistory a.txt: %v", err)
	}
	wantA := []struct {
		checkinRID libfossil.FslID
		action     manifest.FileAction
	}{
		{checkinRID: rid2, action: manifest.FileModified},
		{checkinRID: rid1, action: manifest.FileAdded},
	}
	if len(histA) != len(wantA) {
		t.Fatalf("a.txt: expected %d versions after rebuild, got %d", len(wantA), len(histA))
	}
	for i, want := range wantA {
		if got := histA[i]; got.CheckinRID != want.checkinRID || got.Action != want.action {
			t.Errorf("a.txt version %d = {CheckinRID: %d, Action: %s}, want {CheckinRID: %d, Action: %s}",
				i, got.CheckinRID, got.Action, want.checkinRID, want.action)
		}
	}

	// b.txt: canonical sparse mlinks include it only in the initial commit
	// (pid=0 → added).
	histB, err := manifest.FileHistory(r, manifest.FileHistoryOpts{Path: "b.txt"})
	if err != nil {
		t.Fatalf("FileHistory b.txt: %v", err)
	}
	wantB := []struct {
		checkinRID libfossil.FslID
		action     manifest.FileAction
	}{
		{checkinRID: rid1, action: manifest.FileAdded},
	}
	if len(histB) != len(wantB) {
		t.Fatalf("b.txt: expected %d version after rebuild, got %d", len(wantB), len(histB))
	}
	for i, want := range wantB {
		if got := histB[i]; got.CheckinRID != want.checkinRID || got.Action != want.action {
			t.Errorf("b.txt version %d = {CheckinRID: %d, Action: %s}, want {CheckinRID: %d, Action: %s}",
				i, got.CheckinRID, got.Action, want.checkinRID, want.action)
		}
	}
}

// TestRebuild_DeltaManifest verifies that rebuild correctly handles delta
// manifests (B-card). The delta's F-cards only contain changed files.
// Rebuild should create mlink rows for changed files only — matching
// fossil rebuild behavior (not expanding to the full file set).
func TestRebuild_DeltaManifest(t *testing.T) {
	r := newTestRepo(t)

	// First checkin: two files
	rid1, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("alpha")},
			{Name: "b.txt", Content: []byte("bravo")},
		},
		Comment: "first",
		User:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second checkin: delta manifest (only a.txt changes, b.txt inherited)
	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("alpha modified")},
			{Name: "b.txt", Content: []byte("bravo")},
		},
		Comment: "second (delta)",
		User:    "test",
		Parent:  rid1,
		Delta:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Delete all derived tables
	for _, tbl := range []string{"event", "mlink", "plink", "tagxref", "filename", "leaf", "unclustered", "unsent"} {
		r.DB().Exec("DELETE FROM " + tbl)
	}

	// Rebuild
	_, err = verify.Rebuild(r)
	if err != nil {
		t.Fatal(err)
	}

	// After rebuild, mlink should exist and repo should be clean
	var mlinkCount int
	r.DB().QueryRow("SELECT count(*) FROM mlink").Scan(&mlinkCount)
	if mlinkCount == 0 {
		t.Fatal("expected mlink rows after rebuild with delta manifest")
	}

	// Verify clean
	vReport, err := verify.Verify(r)
	if err != nil {
		t.Fatal(err)
	}
	if !vReport.OK() {
		for _, iss := range vReport.Issues {
			t.Logf("issue: %s", iss.Message)
		}
		t.Fatalf("expected clean verify after delta rebuild, got %d issues", len(vReport.Issues))
	}
}

func TestRebuild_PreservesCanonicalMlinksForLocalMergeWithDeletion(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	rootFiles := []manifest.File{
		{Name: "a.txt", Content: []byte("base")},
		{Name: "delete.txt", Content: []byte("remove me")},
		{Name: "keep-1.txt", Content: []byte("keep 1")},
		{Name: "keep-2.txt", Content: []byte("keep 2")},
		{Name: "keep-3.txt", Content: []byte("keep 3")},
	}
	rootRID, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files:   rootFiles,
		Comment: "root",
		User:    "test",
		Time:    base,
	})
	if err != nil {
		t.Fatalf("root checkin: %v", err)
	}

	trunkRID, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("trunk")},
			{Name: "delete.txt", Content: []byte("remove me")},
			{Name: "keep-1.txt", Content: []byte("keep 1")},
			{Name: "keep-2.txt", Content: []byte("keep 2")},
			{Name: "keep-3.txt", Content: []byte("keep 3")},
		},
		Comment: "trunk change",
		User:    "test",
		Parent:  rootRID,
		Time:    base.Add(time.Minute),
		Delta:   true,
	})
	if err != nil {
		t.Fatalf("trunk checkin: %v", err)
	}

	featureRID, _, err := manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("feature")},
			{Name: "delete.txt", Content: []byte("remove me")},
			{Name: "feature.txt", Content: []byte("feature only")},
			{Name: "keep-1.txt", Content: []byte("keep 1")},
			{Name: "keep-2.txt", Content: []byte("keep 2")},
			{Name: "keep-3.txt", Content: []byte("keep 3")},
		},
		Comment: "feature branch",
		User:    "test",
		Parent:  rootRID,
		Time:    base.Add(2 * time.Minute),
		Delta:   true,
		Tags: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "feature"},
			{Type: deck.TagSingleton, Name: "sym-feature", UUID: "*"},
		},
	})
	if err != nil {
		t.Fatalf("feature checkin: %v", err)
	}

	_, _, err = manifest.Checkin(r, manifest.CheckinOpts{
		Files: []manifest.File{
			{Name: "a.txt", Content: []byte("merged")},
			{Name: "feature.txt", Content: []byte("feature only")},
			{Name: "keep-1.txt", Content: []byte("keep 1")},
			{Name: "keep-2.txt", Content: []byte("keep 2")},
			{Name: "keep-3.txt", Content: []byte("keep 3")},
		},
		Comment:      "merge feature",
		User:         "test",
		Parent:       trunkRID,
		MergeParents: []libfossil.FslID{featureRID},
		Time:         base.Add(3 * time.Minute),
		Delta:        true,
	})
	if err != nil {
		t.Fatalf("merge checkin: %v", err)
	}

	snapshotMlinks := func() (string, int) {
		rows, err := r.DB().Query(`
			SELECT mid, fid, COALESCE(pmid, 0), COALESCE(pid, 0), fnid,
			       COALESCE(pfnid, 0), COALESCE(mperm, 0), COALESCE(isaux, 0)
			FROM mlink
			ORDER BY mid, fid, pmid, pid, fnid, pfnid, mperm, isaux
		`)
		if err != nil {
			t.Fatalf("query mlink snapshot: %v", err)
		}
		defer rows.Close()

		var snapshot bytes.Buffer
		count := 0
		for rows.Next() {
			var mid, fid, pmid, pid, fnid, pfnid, mperm, isaux int64
			if err := rows.Scan(&mid, &fid, &pmid, &pid, &fnid, &pfnid, &mperm, &isaux); err != nil {
				t.Fatalf("scan mlink snapshot: %v", err)
			}
			fmt.Fprintf(&snapshot, "%d,%d,%d,%d,%d,%d,%d,%d\n", mid, fid, pmid, pid, fnid, pfnid, mperm, isaux)
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate mlink snapshot: %v", err)
		}

		digest := sha256.Sum256(snapshot.Bytes())
		return fmt.Sprintf("%x", digest), count
	}

	var auxiliaryRows, deletionRows int
	if err := r.DB().QueryRow("SELECT count(*) FROM mlink WHERE isaux > 0").Scan(&auxiliaryRows); err != nil {
		t.Fatalf("count auxiliary mlinks: %v", err)
	}
	if auxiliaryRows == 0 {
		t.Fatal("expected a positive auxiliary mlink row before rebuild")
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM mlink WHERE fid = 0").Scan(&deletionRows); err != nil {
		t.Fatalf("count deletion mlinks: %v", err)
	}
	if deletionRows == 0 {
		t.Fatal("expected an fid=0 deletion mlink row before rebuild")
	}

	beforeDigest, beforeRows := snapshotMlinks()
	if beforeRows == 0 || beforeDigest == "" {
		t.Fatal("expected a nonempty canonical mlink snapshot before rebuild")
	}

	if _, err := verify.Rebuild(r); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	afterDigest, afterRows := snapshotMlinks()
	if afterRows == 0 || afterDigest == "" {
		t.Fatal("expected a nonempty rebuilt mlink snapshot")
	}
	if afterDigest != beforeDigest {
		t.Fatalf("mlink snapshot digest changed after rebuild: before=%s after=%s", beforeDigest, afterDigest)
	}
}

func TestRebuild_RejectsOversizedCanonicalMlinkSources(t *testing.T) {
	const maxCanonicalMlinkSources = 1024
	const fileUUID = "ffffffffffffffffffffffffffffffffffffffff"

	hash := func(n int) string {
		return fmt.Sprintf("%040x", n)
	}
	tests := []struct {
		name string
		deck func() *deck.Deck
	}{
		{
			name: "primary plus too many merge parents",
			deck: func() *deck.Deck {
				parents := make([]string, 0, maxCanonicalMlinkSources+2)
				for i := range maxCanonicalMlinkSources + 2 {
					parents = append(parents, hash(i+1))
				}
				return &deck.Deck{
					Type: deck.Checkin,
					C:    "too many merge parents",
					D:    time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
					F:    []deck.FileCard{{Name: "file.txt", UUID: fileUUID}},
					P:    parents,
					U:    deck.User("test"),
				}
			},
		},
		{
			name: "too many cherrypick sources",
			deck: func() *deck.Deck {
				cherrypicks := make([]deck.CherryPick, 0, maxCanonicalMlinkSources+1)
				for i := range maxCanonicalMlinkSources + 1 {
					cherrypicks = append(cherrypicks, deck.CherryPick{Target: hash(i + 1)})
				}
				return &deck.Deck{
					Type: deck.Checkin,
					C:    "too many cherrypick sources",
					D:    time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
					F:    []deck.FileCard{{Name: "file.txt", UUID: fileUUID}},
					P:    []string{hash(maxCanonicalMlinkSources + 2)},
					Q:    cherrypicks,
					U:    deck.User("test"),
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRepo(t)
			manifestBytes, err := tc.deck().Marshal()
			if err != nil {
				t.Fatalf("marshal checkin manifest: %v", err)
			}
			if _, _, err := blob.Store(r.DB(), manifestBytes); err != nil {
				t.Fatalf("store checkin manifest: %v", err)
			}

			var rebuildErr error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("Rebuild panicked: %v", recovered)
					}
				}()
				_, rebuildErr = verify.Rebuild(r)
			}()
			if rebuildErr == nil {
				t.Fatal("Rebuild succeeded for an oversized canonical mlink source set")
			}
		})
	}
}

// Rebuild equivalence tests (Go rebuild == fossil rebuild) live in sim/rebuild_test.go.
// They require the fossil binary and belong with other integration tests.
