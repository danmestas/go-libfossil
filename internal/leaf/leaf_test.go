package leaf_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/repo"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
	"github.com/danmestas/go-libfossil/simio"
)

// graph is a repository whose plink and branch rows the test writes by hand,
// so each case states exactly the edges and branches it is about.
type graph struct {
	t *testing.T
	q db.Querier
}

func newGraph(t *testing.T) graph {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leaf.fossil")
	r, err := repo.Create(path, "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return graph{t: t, q: r.DB()}
}

func (g graph) exec(query string, args ...any) {
	g.t.Helper()
	if _, err := g.q.Exec(query, args...); err != nil {
		g.t.Fatalf("%s: %v", query, err)
	}
}

// link adds a plink edge pid -> cid.
func (g graph) link(pid, cid int64, primary bool) {
	g.exec("INSERT INTO plink(pid, cid, isprim, mtime) VALUES(?, ?, ?, 2460000.5)",
		pid, cid, primary)
}

// branch gives rid an active branch row.
func (g graph) branch(rid int64, name string) {
	g.exec("REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) "+
		"VALUES(?, 2, ?, ?, ?, 2460000.5, ?)", leaf.BranchTagID, rid, rid, name, rid)
}

func (g graph) leaves() []int64 {
	g.t.Helper()
	rows, err := g.q.Query("SELECT rid FROM leaf ORDER BY rid")
	if err != nil {
		g.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			g.t.Fatal(err)
		}
		out = append(out, rid)
	}
	return out
}

func (g graph) wantLeaves(stage string, want ...int64) {
	g.t.Helper()
	if got := g.leaves(); !slices.Equal(got, want) {
		g.t.Errorf("%s: leaves = %v, want %v", stage, got, want)
	}
}

// TestPendingChecksOnlyScheduled pins Section 7.3: only a scheduled check-in
// and its parents are rechecked, so a check-in nothing scheduled keeps the
// verdict it had even when a same-branch child has since been linked to it
// (issue #426).
func TestPendingChecksOnlyScheduled(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	p := leaf.NewPending()
	if err := p.Schedule(g.q, 1); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(g.q); err != nil {
		t.Fatal(err)
	}
	p.Reset()
	g.wantLeaves("1 alone", 1)

	// 2 is an untagged child of 1, on the default branch with it. Linking
	// it schedules nothing, so 1 stays a leaf and 2 is never one.
	g.link(1, 2, true)
	if err := p.Apply(g.q); err != nil {
		t.Fatal(err)
	}
	g.wantLeaves("2 linked", 1)

	// A branch row on 2 schedules 2 and its parent 1.
	g.branch(2, "trunk")
	if err := p.Schedule(g.q, 2); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(g.q); err != nil {
		t.Fatal(err)
	}
	g.wantLeaves("2 tagged", 2)
}

// TestPendingChecksParentsAtBothEnds pins leaf_eventually_check: the parents a
// check-in has when it is scheduled are checked even if the batch then moves
// it (a reparent), and so are the parents it has when the schedule runs.
func TestPendingChecksParentsAtBothEnds(t *testing.T) {
	g := newGraph(t)
	for rid := int64(1); rid <= 3; rid++ {
		g.branch(rid, "trunk")
	}
	g.link(1, 2, true)
	g.link(2, 3, true)
	g.exec("INSERT INTO leaf(rid) VALUES(3)")

	p := leaf.NewPending()
	if err := p.Schedule(g.q, 3); err != nil {
		t.Fatal(err)
	}
	// Reparent 3 from 2 to 1. 1 is planted in leaf, stale, so the result
	// shows whether it was checked.
	g.exec("DELETE FROM plink WHERE cid=3")
	g.link(1, 3, true)
	g.exec("INSERT INTO leaf(rid) VALUES(1)")
	if err := p.Apply(g.q); err != nil {
		t.Fatal(err)
	}
	// 2 (the parent at scheduling) is now childless; 1 (the parent now) has
	// same-branch children.
	g.wantLeaves("after apply", 2, 3)
}

// TestPendingRollback pins Mark and Rollback: rechecks scheduled by writes
// that rolled back are forgotten, earlier ones kept.
func TestPendingRollback(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	g.branch(2, "trunk")
	p := leaf.NewPending()
	if err := p.Schedule(g.q, 1); err != nil {
		t.Fatal(err)
	}
	mark := p.Mark()
	if err := p.Schedule(g.q, 2); err != nil {
		t.Fatal(err)
	}
	if p.Len() != 2 {
		t.Fatalf("Len = %d, want 2", p.Len())
	}
	p.Rollback(mark)
	if p.Len() != 1 {
		t.Fatalf("Len after rollback = %d, want 1", p.Len())
	}
	if err := p.Apply(g.q); err != nil {
		t.Fatal(err)
	}
	g.wantLeaves("after apply", 1)
}

// TestRebuildIsSingleBatch pins Section 7.3's full rebuild: the candidates are
// the check-ins with a branch row and the plink parents, so an untagged
// check-in with no child, a candidate of nothing, is not a leaf.
func TestRebuildIsSingleBatch(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	g.link(1, 2, true) // 2 untagged, on trunk by default: 1 is no leaf
	g.branch(3, "feat")
	g.link(2, 3, true) // 2 is a parent of feat's 3: a leaf on trunk
	g.exec("INSERT INTO leaf(rid) VALUES(1)")
	if err := leaf.Rebuild(g.q); err != nil {
		t.Fatal(err)
	}
	g.wantLeaves("after rebuild", 2, 3)
	if err := leaf.Check(g.q, libfossil.FslID(2)); err != nil {
		t.Fatal(err)
	}
	g.wantLeaves("after check", 2, 3)
}

// tagFrom gives rid an active branch row written by the artifact src: 0 for a
// row propagated from a parent, another rid for a control artifact.
func (g graph) tagFrom(rid int64, name string, src int64) {
	g.exec("REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) "+
		"VALUES(?, 2, ?, ?, ?, 2460000.5, ?)", leaf.BranchTagID, src, rid, name, rid)
}

func (g graph) wantAudit(stage string, stale, missing []libfossil.FslID) {
	g.t.Helper()
	g.wantAuditDetached(stage, nil, stale, missing)
}

func (g graph) wantAuditDetached(stage string, detached map[libfossil.FslID]struct{}, stale, missing []libfossil.FslID) {
	g.t.Helper()
	got, err := leaf.Audit(g.q, detached)
	if err != nil {
		g.t.Fatalf("%s: Audit: %v", stage, err)
	}
	if !slices.Equal(got.Stale, stale) || !slices.Equal(got.Missing, missing) {
		g.t.Errorf("%s: Audit = stale %v missing %v, want stale %v missing %v",
			stage, got.Stale, got.Missing, stale, missing)
	}
	if len(got.Stray) != 0 {
		g.t.Errorf("%s: Audit stray %v, want none", stage, got.Stray)
	}
}

// TestAuditAcceptsArrivalOrder pins the tables Section 7.3 allows that the
// test of Section 7.2 alone would reject: an untagged check-in with no child
// may lack a row, and a parent may keep its row when its same-branch child
// carries no branch row, since that child's arrival scheduled nothing.
func TestAuditAcceptsArrivalOrder(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	g.link(1, 2, true) // 2 untagged, on trunk
	g.exec("INSERT INTO leaf(rid) VALUES(1)")
	g.wantAudit("kept verdict", nil, nil)

	// The same repository rebuilt in one batch is just as consistent.
	if err := leaf.Rebuild(g.q); err != nil {
		t.Fatal(err)
	}
	g.wantLeaves("rebuilt")
	g.wantAudit("rebuilt", nil, nil)
}

// TestAuditFollowsBranches pins the Section 7.2 test Audit applies: a merged
// branch tip and a branch point whose children are all on other branches are
// leaves, though each has a child.
func TestAuditFollowsBranches(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	g.branch(2, "feat")
	g.link(1, 2, true) // 1's only child is on feat
	g.branch(3, "trunk")
	g.link(2, 3, false) // 3 merges feat's tip 2 into trunk
	if err := leaf.Rebuild(g.q); err != nil {
		t.Fatal(err)
	}
	g.wantLeaves("rebuilt", 1, 2, 3)
	g.wantAudit("rebuilt", nil, nil)
}

// TestAuditReportsDecidable pins the two findings no arrival order excuses: a
// row whose same-branch child carries a branch row written after the link,
// and a tagged check-in with no same-branch child and no row.
func TestAuditReportsDecidable(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	g.link(1, 2, true)
	g.tagFrom(2, "trunk", 0) // propagated to 2 along the link
	g.branch(3, "feat")
	g.link(1, 3, true)
	g.branch(4, "feat")
	g.link(3, 4, true) // 4's own T card: written as it was linked
	g.exec("INSERT INTO leaf(rid) VALUES(1), (3)")
	g.wantAudit("planted", []libfossil.FslID{1, 3}, []libfossil.FslID{2, 4})

	if err := leaf.Rebuild(g.q); err != nil {
		t.Fatal(err)
	}
	g.wantAudit("rebuilt", nil, nil)
}

// TestAuditToleratesControlTag pins the limit of the stale finding: a branch
// row a control artifact wrote may predate the child's link, when the tag
// arrived before the check-in it names, so it schedules nothing on the parent.
func TestAuditToleratesControlTag(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	g.link(1, 2, true)
	g.tagFrom(2, "trunk", 9)
	g.exec("INSERT INTO leaf(rid) VALUES(1), (2)")
	g.wantAudit("control-tagged child", nil, nil)
}

// TestAuditToleratesReparent pins the reparent exceptions: moving a child
// schedules no recheck, so its new parent may keep a row and its old parent,
// named in detached, may lack one.
func TestAuditToleratesReparent(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	g.branch(2, "feat")
	g.link(1, 2, true)
	g.branch(3, "trunk")
	g.link(1, 3, true) // 3 was 2's child, on trunk by its own T card
	g.parentTag(3, 1)
	g.exec("INSERT INTO leaf(rid) VALUES(1)")
	g.wantAudit("reparented, 2 not detached", nil, []libfossil.FslID{2, 3})
	g.wantAuditDetached("reparented", map[libfossil.FslID]struct{}{2: {}}, nil, []libfossil.FslID{3})
}

// parentTag gives rid a parent tag row of tagtype: 1 for a reparent, 0 once a
// later control artifact cancels it.
func (g graph) parentTag(rid int64, tagtype int) {
	g.exec("INSERT OR IGNORE INTO tag(tagname) VALUES('parent')")
	g.exec("REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) "+
		"SELECT tagid, ?, 9, ?, 'h1', 2460000.5, ? FROM tag WHERE tagname='parent'",
		tagtype, rid, rid)
}

// TestAuditToleratesCancelledReparent pins a cancelled parent tag: the cancel
// re-derives nothing, so the plink row the reparent wrote stays until a full
// rebuild, and its new parent may keep the row it had before.
func TestAuditToleratesCancelledReparent(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	g.branch(2, "feat")
	g.link(1, 2, true)
	g.branch(3, "trunk")
	g.link(1, 3, true)
	g.parentTag(3, 0)
	g.exec("INSERT INTO leaf(rid) VALUES(1)")
	g.wantAuditDetached("cancelled", map[libfossil.FslID]struct{}{2: {}}, nil, []libfossil.FslID{3})
}

// TestAuditReportsStray pins the rows no recheck writes: a rid that is not a
// candidate of Section 7.3 -- no branch row, in no plink row -- such as a
// file artifact or a rid with no artifact at all. A parent a reparent took
// its only child from is excused.
func TestAuditReportsStray(t *testing.T) {
	g := newGraph(t)
	g.branch(1, "trunk")
	g.exec("INSERT INTO leaf(rid) VALUES(1), (7), (424242)")
	got, err := leaf.Audit(g.q, map[libfossil.FslID]struct{}{7: {}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []libfossil.FslID{424242}; !slices.Equal(got.Stray, want) {
		t.Errorf("stray = %v, want %v", got.Stray, want)
	}
	got, err = leaf.Audit(g.q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []libfossil.FslID{7, 424242}; !slices.Equal(got.Stray, want) {
		t.Errorf("stray without detached = %v, want %v", got.Stray, want)
	}
}

func TestAuditDistinctReadingWitnesses(t *testing.T) {
	g := newGraph(t)
	g.exec("REPLACE INTO config(name, value, mtime) VALUES('main-branch', 'main', 0)")
	g.link(1, 2, true)
	g.branch(2, "main")
	g.link(1, 3, false)
	g.branch(3, "trunk")
	g.exec("INSERT INTO leaf(rid) VALUES(1), (2), (3)")
	g.wantAudit("distinct witnesses", []libfossil.FslID{1}, nil)
}

func TestAuditReadingGuards(t *testing.T) {
	for _, branch := range []string{"main", "trunk"} {
		for _, srcid := range []int64{0, 2} {
			t.Run(fmt.Sprintf("%s/srcid%d", branch, srcid), func(t *testing.T) {
				g := newGraph(t)
				g.exec("REPLACE INTO config(name, value, mtime) VALUES('main-branch', 'main', 0)")
				g.link(1, 2, true)
				g.branch(2, branch)
				g.exec("UPDATE tagxref SET srcid=? WHERE rid=2", srcid)
				g.exec("INSERT INTO leaf(rid) VALUES(1), (2)")
				g.wantAudit("only one reading same", nil, nil)
			})
		}
		t.Run(branch+"/missing", func(t *testing.T) {
			g := newGraph(t)
			g.exec("REPLACE INTO config(name, value, mtime) VALUES('main-branch', 'main', 0)")
			g.branch(1, branch)
			g.link(1, 2, true)
			g.exec("INSERT INTO leaf(rid) VALUES(2)")
			g.wantAudit("one reading still has a same-branch child", nil, nil)
		})
	}
	g := newGraph(t)
	g.branch(1, "trunk")
	g.branch(2, "trunk")
	g.exec("UPDATE tagxref SET srcid=0 WHERE rid=2")
	g.link(1, 2, true)
	g.exec("INSERT INTO leaf(rid) VALUES(1), (2)")
	g.wantAudit("srcid zero is direct", []libfossil.FslID{1}, nil)
}

// TestAuditAcceptsFossilReading pins the tables fossil's leaf_check writes
// where it departs from the draft: it reads an untagged check-in as the
// literal trunk whatever main-branch says, and reads a branch row of any
// type, a cancelled one included. A go-built table rechecked under an earlier
// main-branch setting reads the same way. Audit accepts both readings, so a
// row is stale only when both read the child as same-branch, and a row is
// missing only when neither does.
func TestAuditAcceptsFossilReading(t *testing.T) {
	g := newGraph(t)
	g.exec("REPLACE INTO config(name, value, mtime) VALUES('main-branch', 'main', 0)")
	g.link(1, 2, true) // 1 untagged: main to the draft, trunk to fossil
	g.branch(2, "main")
	g.exec("INSERT INTO leaf(rid) VALUES(1), (2)") // fossil's table
	g.wantAudit("main-branch, fossil's table", nil, nil)
	g.exec("DELETE FROM leaf WHERE rid=1") // the draft's table
	g.wantAudit("main-branch, draft's table", nil, nil)
	// 11 is untagged: main to the draft, trunk to fossil. fossil's table
	// lacks 10, which the draft reads as a leaf.
	g.branch(10, "trunk")
	g.link(10, 11, true)
	g.exec("INSERT INTO leaf(rid) VALUES(11)")
	g.wantAudit("main-branch, trunk parent", nil, nil)

	// 3's feat row is cancelled: trunk to the draft, feat to fossil.
	g.exec("DELETE FROM config WHERE name='main-branch'")
	g.exec("REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) "+
		"VALUES(?, 0, 9, 3, 'feat', 2460000.5, 3)", leaf.BranchTagID)
	g.link(3, 4, true)
	g.branch(4, "trunk")
	g.exec("INSERT INTO leaf(rid) VALUES(3), (4)") // fossil's table
	g.wantAudit("cancelled, fossil's table", nil, nil)
	g.exec("DELETE FROM leaf WHERE rid=3") // the draft's table
	g.wantAudit("cancelled, draft's table", nil, nil)
	g.exec("REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) "+
		"VALUES(?, 0, 9, 12, 'feat', 2460000.5, 12)", leaf.BranchTagID)
	g.link(12, 13, true)
	g.branch(13, "feat")
	g.exec("INSERT INTO leaf(rid) VALUES(13)") // fossil's table: 12 is no leaf
	g.wantAudit("cancelled, feat child", nil, nil)

	// Read either way 5 has a same-branch child, and 6 has none.
	g.branch(5, "trunk")
	g.link(5, 6, true)
	g.branch(6, "trunk")
	g.link(6, 7, true)
	g.branch(7, "feat")
	g.exec("INSERT INTO leaf(rid) VALUES(5), (7)")
	g.wantAudit("both readings agree", []libfossil.FslID{5}, []libfossil.FslID{6})
}
