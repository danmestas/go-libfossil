package manifest

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/simio"
)

// mergeMlinkRow is one mlink row keyed the way the assertions below need to read
// it: by the file's name rather than its fnid, and carrying the two columns
// that distinguish a merge-parent transition from a primary one.
type mergeMlinkRow struct {
	pmid  int64
	pid   int64
	fid   int64
	isaux int
	mperm int64
}

// mergeMlinkRowsFor returns every mlink row a check-in owns, keyed by
// (filename, pmid).
func mergeMlinkRowsFor(t *testing.T, q db.Querier, mid libfossil.FslID) map[string]map[int64]mergeMlinkRow {
	t.Helper()
	// isaux is declared BOOLEAN, which one of the two SQLite drivers surfaces
	// as a Go bool; CAST pins it to an integer for both.
	rows, err := q.Query(`SELECT f.name, m.pmid, m.pid, m.fid, CAST(m.isaux AS INTEGER), m.mperm
	                        FROM mlink m JOIN filename f ON f.fnid=m.fnid
	                       WHERE m.mid=?`, mid)
	if err != nil {
		t.Fatalf("query mlink for mid=%d: %v", mid, err)
	}
	defer rows.Close()
	out := map[string]map[int64]mergeMlinkRow{}
	for rows.Next() {
		var name string
		var r mergeMlinkRow
		if err := rows.Scan(&name, &r.pmid, &r.pid, &r.fid, &r.isaux, &r.mperm); err != nil {
			t.Fatalf("scan mlink: %v", err)
		}
		if out[name] == nil {
			out[name] = map[int64]mergeMlinkRow{}
		}
		out[name][r.pmid] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("mlink rows: %v", err)
	}
	return out
}

// TestCrosslinkMergeParentMlinkRows is the issue #193 regression.
//
// A merge check-in has more than one parent, and canonical Fossil records the
// SAME file-level diff once per parent: the primary-parent transition with
// isaux=0, and one auxiliary transition per merge parent with isaux=1 and that
// parent's rid in pmid. Deriving only the primary-parent diff left 3,351 rows
// missing from a clone of the Fossil self-hosting repository -- every one of
// which `fossil rebuild` immediately put back.
//
// The topology below is the smallest one that produces all three row shapes at
// once:
//
//	conflict.txt  edited on BOTH lineages and resolved to a third value in the
//	              merge, so it differs from each parent: one primary row and
//	              one auxiliary row.
//	feature.txt   added on the branch only and carried into the merge
//	              unchanged, so it differs from the primary parent alone: one
//	              primary row, and pid=-1 because it produced fewer rows than
//	              the check-in has parents.
//	trunk.txt     edited on trunk only and carried into the merge unchanged, so
//	              it differs from the merge parent alone -- and canonical
//	              records NOTHING, because an auxiliary row exists only where a
//	              primary row already does.
func TestCrosslinkMergeParentMlinkRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merge.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	body := func(who string, rev int) []byte {
		return []byte(fmt.Sprintf("%s revision %d\n", who, rev))
	}
	commit := func(label string, files []File, parent libfossil.FslID, merges []libfossil.FslID, tags []deck.TagCard, hour int) libfossil.FslID {
		t.Helper()
		rid, _, err := Checkin(r, CheckinOpts{
			Files:        files,
			Comment:      label,
			User:         "testuser",
			Parent:       parent,
			MergeParents: merges,
			Tags:         tags,
			Time:         base.Add(time.Duration(hour) * time.Hour),
		})
		if err != nil {
			t.Fatalf("Checkin(%s): %v", label, err)
		}
		return rid
	}

	root := commit("root", []File{
		{Name: "conflict.txt", Content: body("both", 1)},
		{Name: "trunk.txt", Content: body("trunk", 1)},
	}, 0, nil, nil, 0)

	trunkTip := commit("trunk edit", []File{
		{Name: "conflict.txt", Content: body("trunk", 2)},
		{Name: "trunk.txt", Content: body("trunk", 2)},
	}, root, nil, nil, 1)

	branchTip := commit("branch edit", []File{
		{Name: "conflict.txt", Content: body("branch", 2)},
		{Name: "trunk.txt", Content: body("trunk", 1)},
		{Name: "feature.txt", Content: body("feature", 1)},
	}, root, nil, []deck.TagCard{
		{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "feature-x"},
		{Type: deck.TagSingleton, Name: "sym-feature-x", UUID: "*"},
	}, 2)

	merge := commit("merge branch into trunk", []File{
		{Name: "conflict.txt", Content: body("resolved", 3)},
		{Name: "trunk.txt", Content: body("trunk", 2)},
		{Name: "feature.txt", Content: body("feature", 1)},
	}, trunkTip, []libfossil.FslID{branchTip}, nil, 3)

	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	// Put the repository back in the state a fresh clone is in immediately
	// before crosslinking, then derive everything from the blobs alone.
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	for _, tbl := range crosslinkDerivedTables {
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	r2, err := repo.Open(path)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r2.Close()
	if _, err := Crosslink(r2); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	got := mergeMlinkRowsFor(t, r2.DB(), merge)

	conflict := got["conflict.txt"]
	if len(conflict) != 2 {
		t.Fatalf("conflict.txt: got %d mlink rows %v, want 2 (one per parent transition)", len(conflict), conflict)
	}
	prim, ok := conflict[int64(trunkTip)]
	if !ok {
		t.Fatalf("conflict.txt: no row for the primary parent (pmid=%d): %v", trunkTip, conflict)
	}
	if prim.isaux != 0 {
		t.Errorf("conflict.txt primary row: isaux = %d, want 0", prim.isaux)
	}
	aux, ok := conflict[int64(branchTip)]
	if !ok {
		t.Fatalf("conflict.txt: no row for the merge parent (pmid=%d): %v", branchTip, conflict)
	}
	if aux.isaux != 1 {
		t.Errorf("conflict.txt merge row: isaux = %d, want 1", aux.isaux)
	}
	if aux.fid != prim.fid {
		t.Errorf("conflict.txt merge row: fid = %d, want %d (both rows describe the same child content)", aux.fid, prim.fid)
	}
	if aux.pid == 0 || aux.pid == prim.pid {
		t.Errorf("conflict.txt merge row: pid = %d, want the merge parent's own copy of the file (primary pid is %d)", aux.pid, prim.pid)
	}

	feature := got["feature.txt"]
	if len(feature) != 1 {
		t.Fatalf("feature.txt: got %d mlink rows %v, want 1 (unchanged relative to the merge parent)", len(feature), feature)
	}
	fr, ok := feature[int64(trunkTip)]
	if !ok {
		t.Fatalf("feature.txt: no row for the primary parent (pmid=%d): %v", trunkTip, feature)
	}
	if fr.pid != -1 {
		t.Errorf("feature.txt: pid = %d, want -1 (the file arrived with the merge)", fr.pid)
	}

	if rows, ok := got["trunk.txt"]; ok {
		if _, aux := rows[int64(branchTip)]; aux {
			t.Errorf("trunk.txt: an auxiliary row exists without a primary one: %v", rows)
		}
	}
}

// TestCrosslinkCheckinTagTargetingAnotherArtifact is the tagxref half of issue
// #193.
//
// A check-in's T-card does not have to name the check-in carrying it. `fossil
// merge --integrate` writes the branch it closed as `T +closed <hash>` ON THE
// MERGE CHECK-IN, and canonical Fossil applies a check-in's T-cards exactly as
// it applies a control artifact's. Skipping every card whose UUID was not "*"
// dropped 612 `closed` rows from a clone of the Fossil self-hosting repository.
func TestCrosslinkCheckinTagTargetingAnotherArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tagtarget.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	root, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("one\n")}},
		Comment: "root",
		User:    "testuser",
		Time:    base,
	})
	if err != nil {
		t.Fatalf("Checkin(root): %v", err)
	}
	var rootUUID string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", root).Scan(&rootUUID); err != nil {
		t.Fatalf("root uuid: %v", err)
	}

	// The second check-in closes the first, naming it by hash.
	closer, _, err := Checkin(r, CheckinOpts{
		Files:   []File{{Name: "a.txt", Content: []byte("two\n")}},
		Comment: "close the root",
		User:    "testuser",
		Parent:  root,
		Time:    base.Add(time.Hour),
		Tags: []deck.TagCard{
			{Type: deck.TagPropagating, Name: "branch", UUID: "*", Value: "trunk"},
			{Type: deck.TagSingleton, Name: "closed", UUID: rootUUID},
		},
	})
	if err != nil {
		t.Fatalf("Checkin(closer): %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	for _, tbl := range crosslinkDerivedTables {
		if _, err := d.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	r2, err := repo.Open(path)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer r2.Close()
	if _, err := Crosslink(r2); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	var gotRid, gotSrc int64
	err = r2.DB().QueryRow(`SELECT x.rid, x.srcid FROM tagxref x JOIN tag t ON t.tagid=x.tagid
	                         WHERE t.tagname='closed'`).Scan(&gotRid, &gotSrc)
	if err != nil {
		t.Fatalf("no 'closed' tagxref row was derived (the T-card named another artifact): %v", err)
	}
	if gotRid != int64(root) {
		t.Errorf("closed tag landed on rid %d, want %d (the artifact the T-card names)", gotRid, root)
	}
	if gotSrc != int64(closer) {
		t.Errorf("closed tag srcid = %d, want %d (the check-in carrying the card)", gotSrc, closer)
	}
}
