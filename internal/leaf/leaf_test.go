package leaf_test

import (
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
