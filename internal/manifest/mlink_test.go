package manifest

import (
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
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
	onBranchUUID := repoHash(r, onBranchContent)
	mergeNewContent := []byte("merge-new content")
	mergeNewUUID := repoHash(r, mergeNewContent)
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
	aUUID := repoHash(r, aContent)
	bUUID := repoHash(r, bContent)
	b2UUID := repoHash(r, b2Content)

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
	parentUUID := repoHash(r, parentBytes)

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

// TestInsertCheckinMlinks_FullManifestDeletionByOmission pins issue #157 gap 1:
// a file present in the primary parent but simply ABSENT from a full (non-delta)
// child manifest -- deleted by omission, with no empty-UUID F-card -- must still
// get a delete mlink row (fid=0) whose pid names the parent's blob, matching
// canonical Fossil's add_mlink. The prior code walked only the child's F-cards,
// so an omitted file produced no row at all.
//
// Methodology: hand-build a full parent {a.txt=A, b.txt=B} and a full child
// {a.txt=A} that drops b.txt by omission (b.txt appears in NO F-card, empty or
// otherwise). Store the child blob first so it takes the lower rid and is
// crosslinked before its parent -- the same delta-chain timing #89 exercised --
// then assert the child holds exactly one mlink row: b.txt with fid=0, pmid the
// parent manifest, pid the parent's b.txt blob, and no pfnid.
func TestInsertCheckinMlinks_FullManifestDeletionByOmission(t *testing.T) {
	r := setupTestRepo(t)
	dbq := r.DB()

	aContent := []byte("a content")
	bContent := []byte("b content")
	aUUID := repoHash(r, aContent)
	bUUID := repoHash(r, bContent)

	if _, _, err := blob.Store(dbq, aContent); err != nil {
		t.Fatalf("store a: %v", err)
	}
	bRid, _, err := blob.Store(dbq, bContent)
	if err != nil {
		t.Fatalf("store b: %v", err)
	}

	trunkTags := []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
		{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
	}

	// Parent full manifest: {a.txt=A, b.txt=B}.
	parent := &deck.Deck{
		Type: deck.Checkin,
		C:    "parent",
		D:    time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "a.txt", UUID: aUUID}, {Name: "b.txt", UUID: bUUID}},
		U:    deck.User("tester"),
		T:    trunkTags,
	}
	parentBytes := mustMarshalManifest(t, parent, map[string][]byte{aUUID: aContent, bUUID: bContent})
	parentUUID := repoHash(r, parentBytes)

	// Child full manifest: {a.txt=A}. b.txt is dropped by OMISSION -- there is
	// no F-card for it at all, not even an empty-UUID deletion card.
	child := &deck.Deck{
		Type: deck.Checkin,
		C:    "child drops b.txt by omission",
		D:    time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC),
		P:    []string{parentUUID},
		F:    []deck.FileCard{{Name: "a.txt", UUID: aUUID}},
		U:    deck.User("tester"),
	}
	childBytes := mustMarshalManifest(t, child, map[string][]byte{aUUID: aContent})

	// Store the CHILD before the PARENT so the child is crosslinked first --
	// pid must still come from the parent MANIFEST, not the not-yet-written
	// parent mlink rows.
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

	// The child must have exactly one mlink row: the b.txt deletion. a.txt is
	// carried over unchanged and must not appear.
	var childMlinkCount int
	if err := dbq.QueryRow("SELECT count(*) FROM mlink WHERE mid=?", childRid).Scan(&childMlinkCount); err != nil {
		t.Fatalf("count child mlink: %v", err)
	}
	if childMlinkCount != 1 {
		t.Errorf("child mlink count = %d, want 1 (only the b.txt deletion by omission)", childMlinkCount)
	}

	var aCount int
	if err := dbq.QueryRow(
		`SELECT count(*) FROM mlink m JOIN filename f USING(fnid) WHERE m.mid=? AND f.name='a.txt'`,
		childRid).Scan(&aCount); err != nil {
		t.Fatalf("count a.txt mlink: %v", err)
	}
	if aCount != 0 {
		t.Errorf("a.txt mlink rows = %d, want 0 (carried over unchanged)", aCount)
	}

	var row mlinkRow
	var pfnid int64
	if err := dbq.QueryRow(
		`SELECT m.fid, m.pmid, m.pid, m.fnid, coalesce(m.pfnid,0) FROM mlink m
		 JOIN filename f USING(fnid) WHERE m.mid=? AND f.name='b.txt'`,
		childRid).Scan(&row.fid, &row.pmid, &row.pid, &row.fnid, &pfnid); err != nil {
		t.Fatalf("b.txt mlink row: %v", err)
	}
	if row.fid != 0 {
		t.Errorf("b.txt fid = %d, want 0 (deleted by omission)", row.fid)
	}
	if row.pmid != int64(parentRid) {
		t.Errorf("b.txt pmid = %d, want %d (primary parent manifest)", row.pmid, parentRid)
	}
	if row.pid != int64(bRid) {
		t.Errorf("b.txt pid = %d, want %d (parent's b.txt blob rid)", row.pid, bRid)
	}
	if pfnid != 0 {
		t.Errorf("b.txt pfnid = %d, want 0 (a deletion, not a rename)", pfnid)
	}
}

// TestInsertCheckinMlinks_RenameResolvesPidFromOldPath pins issue #157 gap 2:
// the rename row (keyed on the new name) must resolve its pid from the parent's
// blob at the OLD path, not default to 0 as a fresh add would. The fixture
// renames-with-edit so pid (old blob) and fid (new blob) are distinct rids,
// making the "pid comes from the old path" claim unambiguous.
//
// Canonical Fossil emits TWO rows for a rename, confirmed against the fossil
// binary in TestFossilBinaryMlinkParity_DeletionAndRename: the rename row under
// the new name (pfnid = old name) and a delete row (fid=0) for the vacated old
// name. Both carry pid = the parent's old-path blob rid.
//
// Methodology: parent full {old.txt=X}; child full renames old.txt -> new.txt
// while editing its content to Y, as a single F-card {Name:new.txt, UUID:Y,
// OldName:old.txt}. Assert both rows: new.txt with fid=Y, pid=X, pfnid=old.txt;
// and old.txt with fid=0, pid=X, no pfnid.
func TestInsertCheckinMlinks_RenameResolvesPidFromOldPath(t *testing.T) {
	r := setupTestRepo(t)
	dbq := r.DB()

	xContent := []byte("original content")
	yContent := []byte("edited content after rename")
	xUUID := repoHash(r, xContent)
	yUUID := repoHash(r, yContent)

	xRid, _, err := blob.Store(dbq, xContent)
	if err != nil {
		t.Fatalf("store x: %v", err)
	}
	yRid, _, err := blob.Store(dbq, yContent)
	if err != nil {
		t.Fatalf("store y: %v", err)
	}

	trunkTags := []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
		{Type: deck.TagSingleton, Name: "sym-trunk", UUID: "*"},
	}

	// Parent full manifest: {old.txt=X}.
	parent := &deck.Deck{
		Type: deck.Checkin,
		C:    "parent",
		D:    time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		F:    []deck.FileCard{{Name: "old.txt", UUID: xUUID}},
		U:    deck.User("tester"),
		T:    trunkTags,
	}
	parentBytes := mustMarshalManifest(t, parent, map[string][]byte{xUUID: xContent})
	parentUUID := repoHash(r, parentBytes)

	// Child full manifest: old.txt renamed to new.txt AND edited to Y, as one
	// F-card carrying the prior name.
	child := &deck.Deck{
		Type: deck.Checkin,
		C:    "child renames and edits old.txt",
		D:    time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC),
		P:    []string{parentUUID},
		F:    []deck.FileCard{{Name: "new.txt", UUID: yUUID, OldName: "old.txt"}},
		U:    deck.User("tester"),
	}
	childBytes := mustMarshalManifest(t, child, map[string][]byte{yUUID: yContent})

	childRid, _, err := blob.Store(dbq, childBytes)
	if err != nil {
		t.Fatalf("store child: %v", err)
	}
	if _, _, err := blob.Store(dbq, parentBytes); err != nil {
		t.Fatalf("store parent: %v", err)
	}

	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	if linked != 2 {
		t.Fatalf("Crosslink linked = %d, want 2 (parent + child)", linked)
	}

	// Two rows: the rename (new.txt) and the delete of the vacated old.txt.
	var childMlinkCount int
	if err := dbq.QueryRow("SELECT count(*) FROM mlink WHERE mid=?", childRid).Scan(&childMlinkCount); err != nil {
		t.Fatalf("count child mlink: %v", err)
	}
	if childMlinkCount != 2 {
		t.Errorf("child mlink count = %d, want 2 (the rename row plus the old-name delete)", childMlinkCount)
	}

	var oldFnid int64
	if err := dbq.QueryRow(`SELECT fnid FROM filename WHERE name='old.txt'`).Scan(&oldFnid); err != nil {
		t.Fatalf("old.txt fnid lookup: %v", err)
	}

	// The rename row, keyed on the new name: fid=Y, pid=X (old path), pfnid=old.
	var renameRow mlinkRow
	var renamePfnid int64
	if err := dbq.QueryRow(
		`SELECT m.fid, m.pid, m.fnid, coalesce(m.pfnid,0) FROM mlink m
		 JOIN filename f USING(fnid) WHERE m.mid=? AND f.name='new.txt'`,
		childRid).Scan(&renameRow.fid, &renameRow.pid, &renameRow.fnid, &renamePfnid); err != nil {
		t.Fatalf("new.txt mlink row: %v", err)
	}
	if renameRow.fid != int64(yRid) {
		t.Errorf("new.txt fid = %d, want %d (child's new blob)", renameRow.fid, yRid)
	}
	if renameRow.pid != int64(xRid) {
		t.Errorf("new.txt pid = %d, want %d (parent's OLD-path blob rid, not 0)", renameRow.pid, xRid)
	}
	if renamePfnid != oldFnid {
		t.Errorf("new.txt pfnid = %d, want %d (old.txt's fnid)", renamePfnid, oldFnid)
	}

	// The delete row for the vacated old name: fid=0, pid=X, no pfnid.
	var delRow mlinkRow
	var delPfnid int64
	if err := dbq.QueryRow(
		`SELECT m.fid, m.pid, m.fnid, coalesce(m.pfnid,0) FROM mlink m
		 WHERE m.mid=? AND m.fnid=?`,
		childRid, oldFnid).Scan(&delRow.fid, &delRow.pid, &delRow.fnid, &delPfnid); err != nil {
		t.Fatalf("old.txt delete mlink row: %v", err)
	}
	if delRow.fid != 0 {
		t.Errorf("old.txt fid = %d, want 0 (vacated by the rename)", delRow.fid)
	}
	if delRow.pid != int64(xRid) {
		t.Errorf("old.txt pid = %d, want %d (parent's old.txt blob rid)", delRow.pid, xRid)
	}
	if delPfnid != 0 {
		t.Errorf("old.txt pfnid = %d, want 0 (a deletion, not a rename)", delPfnid)
	}
}
