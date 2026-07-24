package manifest

import (
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// mlinkRow mirrors one row of the mlink table for assertions below.
type mlinkRow struct {
	fid, pmid, pid, fnid int64
}

// TestPermToMperm_SubstringMatch pins permToMperm to canonical Fossil's
// substring test (manifest_file_mperm, src/manifest.c:1482-1492), not an
// exact-string match. "wx" is the case that matters in practice:
// internal/deck/parse.go:194 assigns the F-card perm field verbatim from
// remote xfer input, and canonical Fossil emits multi-character perm
// fields (e.g. the " w" rename placeholder from #51). An exact match on
// "wx" would silently drop the executable bit — the invariant #48
// protects.
func TestPermToMperm_SubstringMatch(t *testing.T) {
	cases := []struct {
		perm string
		want int64
	}{
		{"", 0},
		{"w", 0},
		{"x", 1},
		{"l", 2},
		{"wx", 1}, // multi-character perm containing x: must still map to exec
		{"xl", 1}, // x wins over l when both present, matching canonical's check order
		{"lx", 1}, // order within the string must not matter
		{" w", 0}, // #51's rename placeholder: no x or l present
	}
	for _, c := range cases {
		if got := permToMperm(c.perm); got != c.want {
			t.Errorf("permToMperm(%q) = %d, want %d", c.perm, got, c.want)
		}
	}
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
		U: deck.User("tester"),
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

	// rowFor takes the subtest's own *testing.T (not the parent's) so a
	// Fatalf in one subtest's lookup cannot abort its siblings: t.Run
	// subtests share the parent goroutine, and Fatalf unwinds via
	// runtime.Goexit, which stops the entire enclosing test function if the
	// closure captured the parent t instead of the subtest's.
	rowFor := func(t *testing.T, name string) mlinkRow {
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
		row := rowFor(t, "root.txt")
		if row.fid != 0 {
			t.Errorf("root.txt fid = %d, want 0 (deleted)", row.fid)
		}
		if row.pmid != int64(trunkRid) {
			t.Errorf("root.txt pmid = %d, want %d (primary parent)", row.pmid, trunkRid)
		}
		var trunkFid int64
		if err := d.QueryRow(
			`SELECT m.fid FROM mlink m JOIN filename f USING(fnid) WHERE m.mid=? AND f.name='root.txt'`,
			trunkRid,
		).Scan(&trunkFid); err != nil {
			t.Fatalf("trunk root.txt fid lookup: %v", err)
		}
		if row.pid != trunkFid {
			t.Errorf("root.txt pid = %d, want %d (the primary parent's file rid)", row.pid, trunkFid)
		}
	})

	t.Run("merge_added_file_gets_pid_negative_one", func(t *testing.T) {
		row := rowFor(t, "on-branch.txt")
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
		row := rowFor(t, "merge-new.txt")
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

// mustMarshalManifest computes a manifest's R-card over blobs and marshals it
// to its on-disk bytes, failing the test on any error. blobs maps every file
// UUID the R-card walks to its content.
func mustMarshalManifest(t *testing.T, d *deck.Deck, blobs map[string][]byte) []byte {
	t.Helper()
	rHash, err := d.ComputeR(func(uuid string) ([]byte, error) {
		if c, ok := blobs[uuid]; ok {
			return c, nil
		}
		return nil, fmt.Errorf("unexpected uuid: %s", uuid)
	})
	if err != nil {
		t.Fatalf("ComputeR: %v", err)
	}
	d.R = rHash
	b, err := d.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

// TestInsertCheckinMlinks_DiffsAgainstParentManifest pins the #89 mlink
// correctness fix on the Crosslink (xfer ingestion) write path: an mlink row
// must be emitted only for a file that CHANGED relative to its primary
// parent's manifest -- added, modified, deleted, or renamed -- not one row per
// F-card. pmid/pid must be populated from the parent MANIFEST, so pid is
// correct even when the parent check-in has not been crosslinked yet, which is
// routine under the delta-chain visiting order that crosslinks a child before
// its parent.
//
// Methodology: hand-build a parent baseline manifest {a.txt=A, b.txt=B} and a
// child {a.txt=A (unchanged), b.txt=B2 (modified)} whose P-card names the
// parent. Store the CHILD manifest blob FIRST so it takes the lower rid and is
// visited before the parent in the same Crosslink sweep -- the exact timing
// under which the old parent-mlink lookup returned an empty set and defaulted
// pid to 0. Assert the child's mlink holds exactly one row (b.txt), none for
// the unchanged a.txt, and that b.txt's pmid/pid name the parent manifest and
// the parent's b.txt blob.
func TestInsertCheckinMlinks_DiffsAgainstParentManifest(t *testing.T) {
	r := setupTestRepo(t)
	dbq := r.DB()

	aContent := []byte("a content")
	bContent := []byte("b content v1")
	b2Content := []byte("b content v2")
	aUUID := hash.SHA1(aContent)
	bUUID := hash.SHA1(bContent)
	b2UUID := hash.SHA1(b2Content)

	// Store every file blob up front so Crosslink never defers.
	if _, _, err := blob.Store(dbq, aContent); err != nil {
		t.Fatalf("store a: %v", err)
	}
	bRid, _, err := blob.Store(dbq, bContent)
	if err != nil {
		t.Fatalf("store b: %v", err)
	}
	b2Rid, _, err := blob.Store(dbq, b2Content)
	if err != nil {
		t.Fatalf("store b2: %v", err)
	}

	trunkTags := []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
		{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
	}

	// Parent baseline manifest: {a.txt=A, b.txt=B}.
	parent := &deck.Deck{
		Type: deck.Checkin,
		C:    "parent",
		D:    time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "a.txt", UUID: aUUID}, {Name: "b.txt", UUID: bUUID}},
		U:    deck.User("tester"),
		T:    trunkTags,
	}
	parentBytes := mustMarshalManifest(t, parent, map[string][]byte{aUUID: aContent, bUUID: bContent})
	parentUUID := hash.SHA1(parentBytes)

	// Child baseline manifest atop the parent: a.txt unchanged, b.txt modified.
	child := &deck.Deck{
		Type: deck.Checkin,
		C:    "child modifies b.txt",
		D:    time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC),
		P:    []string{parentUUID},
		F:    []deck.FileCard{{Name: "a.txt", UUID: aUUID}, {Name: "b.txt", UUID: b2UUID}},
		U:    deck.User("tester"),
	}
	childBytes := mustMarshalManifest(t, child, map[string][]byte{aUUID: aContent, b2UUID: b2Content})

	// Store the CHILD blob before the PARENT blob so the child gets the lower
	// rid and is crosslinked first -- reproducing the parent-not-yet-linked
	// timing that made the old code default pid to 0.
	childRid, _, err := blob.Store(dbq, childBytes)
	if err != nil {
		t.Fatalf("store child: %v", err)
	}
	parentRid, _, err := blob.Store(dbq, parentBytes)
	if err != nil {
		t.Fatalf("store parent: %v", err)
	}

	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked != 2 {
		t.Fatalf("Crosslink linked = %d, want 2 (parent + child)", linked)
	}

	// The child must have exactly one mlink row: b.txt (modified). a.txt is
	// unchanged from the parent and must not appear.
	var childMlinkCount int
	if err := dbq.QueryRow("SELECT count(*) FROM mlink WHERE mid=?", childRid).Scan(&childMlinkCount); err != nil {
		t.Fatalf("count child mlink: %v", err)
	}
	if childMlinkCount != 1 {
		t.Errorf("child mlink count = %d, want 1 (only the modified b.txt)", childMlinkCount)
	}

	var aCount int
	if err := dbq.QueryRow(
		`SELECT count(*) FROM mlink m JOIN filename f USING(fnid) WHERE m.mid=? AND f.name='a.txt'`,
		childRid).Scan(&aCount); err != nil {
		t.Fatalf("count a.txt mlink: %v", err)
	}
	if aCount != 0 {
		t.Errorf("a.txt mlink rows = %d, want 0 (unchanged from parent)", aCount)
	}

	var row mlinkRow
	if err := dbq.QueryRow(
		`SELECT m.fid, m.pmid, m.pid, m.fnid FROM mlink m JOIN filename f USING(fnid) WHERE m.mid=? AND f.name='b.txt'`,
		childRid).Scan(&row.fid, &row.pmid, &row.pid, &row.fnid); err != nil {
		t.Fatalf("b.txt mlink row: %v", err)
	}
	if row.fid != int64(b2Rid) {
		t.Errorf("b.txt fid = %d, want %d (child's new blob)", row.fid, b2Rid)
	}
	if row.pmid != int64(parentRid) {
		t.Errorf("b.txt pmid = %d, want %d (primary parent manifest)", row.pmid, parentRid)
	}
	if row.pid != int64(bRid) {
		t.Errorf("b.txt pid = %d, want %d (parent's b.txt blob); pid must come from the parent manifest, not default to 0", row.pid, bRid)
	}
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

	// rowFor takes the subtest's own *testing.T so a Fatalf in one
	// subtest's lookup cannot abort its siblings (see the twin helper in
	// TestInsertCheckinMlinks_ThreeCasePidRule for the mechanism).
	rowFor := func(t *testing.T, name string) mlinkRow {
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
		row := rowFor(t, "on-branch.txt")
		if row.pid != -1 {
			t.Errorf("on-branch.txt pid = %d, want -1 (added by merge)", row.pid)
		}
		if row.pmid != int64(trunkRid) {
			t.Errorf("on-branch.txt pmid = %d, want %d (primary parent)", row.pmid, trunkRid)
		}
	})

	t.Run("normal_added_file_gets_pid_zero", func(t *testing.T) {
		row := rowFor(t, "brand-new.txt")
		if row.pid != 0 {
			t.Errorf("brand-new.txt pid = %d, want 0", row.pid)
		}
	})

	t.Run("carried_over_file_gets_parent_fid", func(t *testing.T) {
		row := rowFor(t, "root.txt")
		var trunkFid int64
		if err := d.QueryRow(
			`SELECT m.fid FROM mlink m JOIN filename f USING(fnid) WHERE m.mid=? AND f.name='root.txt'`,
			trunkRid,
		).Scan(&trunkFid); err != nil {
			t.Fatalf("trunk root.txt fid lookup: %v", err)
		}
		if row.pid != trunkFid {
			t.Errorf("root.txt pid = %d, want %d (the primary parent's file rid, carried over unchanged)", row.pid, trunkFid)
		}
		if row.pmid != int64(trunkRid) {
			t.Errorf("root.txt pmid = %d, want %d", row.pmid, trunkRid)
		}
	})
}
