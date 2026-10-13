package manifest

import (
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// TestRepairLeafTableOnlyHoldsCheckins pins Section 7.1: a leaf is a check-in.
// fossil writes plink rows for forum posts and wiki edits that carry a P-card,
// so a candidate set drawn from plink children alone puts them in the table.
func TestRepairLeafTableOnlyHoldsCheckins(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c1 := storeCheckin(t, r, "c1", "trunk", base)
	storeCheckin(t, r, "c2", "", base.Add(time.Minute), c1)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	var parent int64
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", c1).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	for i, typ := range []string{"f", "w"} {
		rid, _, err := blob.Store(r.DB(), []byte{byte('a' + i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.DB().Exec("INSERT INTO event(type,mtime,objid,user,comment) "+
			"VALUES(?,2460000.5,?,'u','x')", typ, int64(rid)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.DB().Exec("INSERT INTO plink(pid,cid,isprim,mtime) "+
			"VALUES(?,?,1,2460000.5)", parent, int64(rid)); err != nil {
			t.Fatal(err)
		}
	}
	if err := repairLeafTable(r.DB()); err != nil {
		t.Fatalf("repairLeafTable: %v", err)
	}
	var n int
	if err := r.DB().QueryRow("SELECT count(*) FROM leaf JOIN event ON objid=rid " +
		"WHERE type<>'ci'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d non-check-in rows in leaf", n)
	}
	assertLeaves(t, r, "after repair", []string{"c2"})
}

// TestLeafUntaggedChildUsesMainBranch pins Section 6.1: with main-branch set,
// a check-in with no branch tag is on that branch, so it joins its parent
// there and the parent is no leaf, by the table recompute and by leafCheck.
func TestLeafUntaggedChildUsesMainBranch(t *testing.T) {
	r := setupTestRepo(t)
	if err := r.SetConfig("main-branch", "main"); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c1 := storeCheckin(t, r, "c1", "main", base)
	storeCheckin(t, r, "c2", "", base.Add(time.Minute), c1)
	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}
	// Propagation gave c2 the branch; drop it so c2 carries no active branch
	// row, as a check-in with a non-propagating parent tag does.
	if _, err := r.DB().Exec("DELETE FROM tagxref WHERE tagid=? AND rid=("+
		"SELECT objid FROM event WHERE comment='c2')", branchTagID); err != nil {
		t.Fatal(err)
	}
	if err := repairLeafTable(r.DB()); err != nil {
		t.Fatal(err)
	}
	// c2 is never rechecked (it has no tag, as in fossil), so only c1's
	// verdict is at stake: it has a same-branch child and is no leaf.
	assertLeaves(t, r, "after repair", nil)

	var rid int64
	if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", c1).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB().Exec("INSERT OR IGNORE INTO leaf(rid) VALUES(?)", rid); err != nil {
		t.Fatal(err)
	}
	if err := leafCheck(r.DB(), libfossil.FslID(rid)); err != nil {
		t.Fatal(err)
	}
	assertLeaves(t, r, "after leafCheck", nil)
}
