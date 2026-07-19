package manifest

import (
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/deck"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/hash"
)

// mlinkRow mirrors one row of the mlink table for assertions below.
type mlinkRow struct {
	fid, pmid, pid, fnid int64
}

// TestInsertCheckinMlinks_ThreeCasePidRule exercises libfossil#29's
// acceptance criteria directly against the Crosslink (xfer ingestion)
// write path: a merge commit whose F-cards cover all three pid cases from
// canonical Fossil's add_mlink comment (src/manifest.c:1668-1679), plus the
// fid=0 deletion case.
//
//   - root.txt:      deleted by the merge commit           -> fid=0
//   - on-branch.txt: absent from the primary parent (trunk) but present in
//     the merge parent (feature)                            -> pid=-1
//   - merge-new.txt: absent from every parent                -> pid=0
//   - (implicitly) any file carried unchanged from the primary parent
//     resolves to pid = the parent's fid, exercised by every other
//     Crosslink test in this package.
func TestInsertCheckinMlinks_ThreeCasePidRule(t *testing.T) {
	r := setupTestRepo(t)
	d := r.DB()

	// c1: trunk seed via the direct check-in path.
	rootContent := []byte("root content")
	trunkRid, trunkUUID, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "root.txt", Content: rootContent}},
		Comment: "trunk seed",
		User:    "tester",
		Time:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("trunk Checkin: %v", err)
	}

	// c2: feature branch off c1, adding on-branch.txt.
	onBranchContent := []byte("on-branch content")
	_, featureUUID, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "root.txt", Content: rootContent},
			{Name: "on-branch.txt", Content: onBranchContent},
		},
		Parent:  trunkRid,
		Comment: "feature adds on-branch.txt",
		User:    "tester",
		Time:    time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("feature Checkin: %v", err)
	}

	// c3: hand-built merge commit, ingested via Crosslink (the xfer path),
	// covering all three pid cases plus a deletion in one check-in:
	//   - root.txt:      deleted (empty UUID F-card)         -> fid=0
	//   - on-branch.txt: unchanged from the merge parent       -> pid=-1
	//   - merge-new.txt: brand new, in neither parent          -> pid=0
	onBranchUUID := hash.SHA1(onBranchContent)
	mergeNewContent := []byte("merge-new content")
	mergeNewUUID := hash.SHA1(mergeNewContent)
	if _, _, err := blob.Store(d, mergeNewContent); err != nil {
		t.Fatalf("blob.Store(merge-new.txt): %v", err)
	}

	mergeDeck := &deck.Deck{
		Type: deck.Checkin,
		C:    "merge feature into trunk",
		D:    time.Date(2026, 5, 1, 14, 0, 0, 0, time.UTC),
		P:    []string{trunkUUID, featureUUID}, // primary=trunk, merge=feature
		F: []deck.FileCard{
			{Name: "root.txt"}, // deleted: no UUID
			{Name: "on-branch.txt", UUID: onBranchUUID},
			{Name: "merge-new.txt", UUID: mergeNewUUID},
		},
		U: "tester",
	}
	rHash, err := mergeDeck.ComputeR(func(uuid string) ([]byte, error) {
		switch uuid {
		case onBranchUUID:
			return onBranchContent, nil
		case mergeNewUUID:
			return mergeNewContent, nil
		default:
			return nil, fmt.Errorf("unexpected uuid: %s", uuid)
		}
	})
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	mergeDeck.R = rHash
	mergeBytes, err := mergeDeck.Marshal()
	if err != nil {
		t.Fatalf("Marshal merge: %v", err)
	}
	mergeRid, _, err := blob.Store(d, mergeBytes)
	if err != nil {
		t.Fatalf("blob.Store(merge): %v", err)
	}

	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked != 1 {
		t.Fatalf("Crosslink linked = %d, want 1 (merge manifest only; trunk/feature already crosslinked by Checkin)", linked)
	}

	rowFor := func(name string) mlinkRow {
		t.Helper()
		var row mlinkRow
		err := d.QueryRow(
			`SELECT m.fid, m.pmid, m.pid, m.fnid FROM mlink m
			 JOIN filename f USING(fnid) WHERE m.mid=? AND f.name=?`,
			mergeRid, name,
		).Scan(&row.fid, &row.pmid, &row.pid, &row.fnid)
		if err != nil {
			t.Fatalf("mlink row for %q: %v", name, err)
		}
		return row
	}

	t.Run("deleted_file_gets_fid_zero", func(t *testing.T) {
		row := rowFor("root.txt")
		if row.fid != 0 {
			t.Errorf("root.txt fid = %d, want 0 (deleted)", row.fid)
		}
		if row.pmid != int64(trunkRid) {
			t.Errorf("root.txt pmid = %d, want %d (primary parent)", row.pmid, trunkRid)
		}
		if row.pid == 0 {
			t.Errorf("root.txt pid = 0, want the primary parent's file rid (root.txt existed in trunk)")
		}
	})

	t.Run("merge_added_file_gets_pid_negative_one", func(t *testing.T) {
		row := rowFor("on-branch.txt")
		if row.pid != -1 {
			t.Errorf("on-branch.txt pid = %d, want -1 (added by merge)", row.pid)
		}
		if row.pmid != int64(trunkRid) {
			t.Errorf("on-branch.txt pmid = %d, want %d (primary parent, even though pid resolves via the merge parent)", row.pmid, trunkRid)
		}
		if row.fid == 0 {
			t.Errorf("on-branch.txt fid = 0, want the merge commit's file rid")
		}
	})

	t.Run("normal_added_file_gets_pid_zero", func(t *testing.T) {
		row := rowFor("merge-new.txt")
		if row.pid != 0 {
			t.Errorf("merge-new.txt pid = %d, want 0 (new to this check-in, absent from every parent)", row.pid)
		}
		if row.pmid != int64(trunkRid) {
			t.Errorf("merge-new.txt pmid = %d, want %d (primary parent)", row.pmid, trunkRid)
		}
		if row.fid == 0 {
			t.Errorf("merge-new.txt fid = 0, want the merge commit's file rid")
		}
	})
}

// TestInsertMlinks_MergeParentsGetPidNegativeOne exercises the SAME
// three-case pid rule on the direct check-in path (insertMlinks in
// manifest.go), confirming resolveMlinkParent produces identical
// pid/pmid semantics regardless of which write path calls it.
func TestInsertMlinks_MergeParentsGetPidNegativeOne(t *testing.T) {
	r := setupTestRepo(t)
	d := r.DB()

	rootContent := []byte("root content")
	trunkRid, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "root.txt", Content: rootContent}},
		Comment: "trunk seed",
		User:    "tester",
		Time:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("trunk Checkin: %v", err)
	}

	onBranchContent := []byte("on-branch content")
	featureRid, _, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "root.txt", Content: rootContent},
			{Name: "on-branch.txt", Content: onBranchContent},
		},
		Parent:  trunkRid,
		Comment: "feature adds on-branch.txt",
		User:    "tester",
		Time:    time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("feature Checkin: %v", err)
	}

	// Merge commit via the direct path: primary=trunk, merge parent=feature.
	// on-branch.txt is new relative to trunk but already exists on feature,
	// so it must resolve to pid=-1; brand-new.txt exists in neither
	// parent, so it must resolve to pid=0.
	mergeNewContent := []byte("brand new content")
	mergeRid, _, err := Checkin(r, CheckinOpts{
		Files: []File{
			{Name: "root.txt", Content: rootContent},
			{Name: "on-branch.txt", Content: onBranchContent},
			{Name: "brand-new.txt", Content: mergeNewContent},
		},
		Parent:       trunkRid,
		MergeParents: []libfossil.FslID{featureRid},
		Comment:      "merge feature into trunk",
		User:         "tester",
		Time:         time.Date(2026, 5, 1, 14, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("merge Checkin: %v", err)
	}

	rowFor := func(name string) mlinkRow {
		t.Helper()
		var row mlinkRow
		err := d.QueryRow(
			`SELECT m.fid, m.pmid, m.pid, m.fnid FROM mlink m
			 JOIN filename f USING(fnid) WHERE m.mid=? AND f.name=?`,
			mergeRid, name,
		).Scan(&row.fid, &row.pmid, &row.pid, &row.fnid)
		if err != nil {
			t.Fatalf("mlink row for %q: %v", name, err)
		}
		return row
	}

	t.Run("merge_added_file_gets_pid_negative_one", func(t *testing.T) {
		row := rowFor("on-branch.txt")
		if row.pid != -1 {
			t.Errorf("on-branch.txt pid = %d, want -1 (added by merge)", row.pid)
		}
		if row.pmid != int64(trunkRid) {
			t.Errorf("on-branch.txt pmid = %d, want %d (primary parent)", row.pmid, trunkRid)
		}
	})

	t.Run("normal_added_file_gets_pid_zero", func(t *testing.T) {
		row := rowFor("brand-new.txt")
		if row.pid != 0 {
			t.Errorf("brand-new.txt pid = %d, want 0", row.pid)
		}
	})

	t.Run("carried_over_file_gets_parent_fid", func(t *testing.T) {
		row := rowFor("root.txt")
		if row.pid == 0 {
			t.Errorf("root.txt pid = 0, want the primary parent's file rid (carried over unchanged)")
		}
		if row.pmid != int64(trunkRid) {
			t.Errorf("root.txt pmid = %d, want %d", row.pmid, trunkRid)
		}
	})
}
