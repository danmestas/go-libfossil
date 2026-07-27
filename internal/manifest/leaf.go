package manifest

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// Leaf maintenance, in one place so the sweep and the local commit path cannot
// drift apart on what a leaf is.
//
// Canonical fossil's rule (src/leaf.c, leaf_check/is_a_leaf/leaf_rebuild): a
// check-in stops being a leaf only once a child of it carries the *same* branch
// name, where a check-in with no branch tag reads as "trunk":
//
//	SELECT 1 FROM plink
//	 WHERE pid=:rid
//	   AND coalesce((SELECT value FROM tagxref
//	                  WHERE tagid=TAG_BRANCH AND rid=plink.pid),'trunk')
//	    == coalesce((SELECT value FROM tagxref
//	                  WHERE tagid=TAG_BRANCH AND rid=plink.cid),'trunk')
//
// Two consequences are easy to get wrong and are what issue #189 was:
//
//   - The merge parent of a check-in on another branch stays a leaf. A branch
//     merged back into trunk keeps its tip in the leaf table -- that class was
//     939 of the 1,426 leaves the real Fossil repository has.
//   - The plink row's isprim flag is not consulted at all. A branch point stays
//     a leaf even though its only child is its *primary* child, because that
//     child is on a different branch.
//
// The branch comparison reads tagxref, so both entry points must run after the
// branch tag of every check-in involved is in tagxref -- including the ones a
// check-in inherits by propagation rather than declaring itself.
//
// branchTagID is the tagid db/schema.go seeds for 'branch', which is fossil's
// own TAG_BRANCH; both schemas fix it, so it is a constant here rather than a
// lookup per row of a whole-repository recompute.
const branchTagID = 8

// leafCheck brings rid's leaf row in line with the rule above, inserting or
// deleting as needed. It is fossil's leaf_check(), and like fossil's it is
// called on a check-in and on each of its parents once the surrounding write is
// otherwise complete (leaf_eventually_check + leaf_do_pending_checks).
func leafCheck(q db.Querier, rid libfossil.FslID) error {
	if q == nil {
		panic("manifest.leafCheck: q must not be nil")
	}
	if rid <= 0 {
		panic("manifest.leafCheck: rid must be positive")
	}

	var one int
	err := q.QueryRow(fmt.Sprintf(`
		SELECT 1 FROM plink
		 WHERE plink.pid = ?
		   AND coalesce((SELECT value FROM tagxref
		                  WHERE tagid=%d AND rid=plink.pid),'trunk')
		     = coalesce((SELECT value FROM tagxref
		                  WHERE tagid=%d AND rid=plink.cid),'trunk')
		 LIMIT 1`, branchTagID, branchTagID), rid).Scan(&one)
	switch {
	case err == nil:
		if _, err := q.Exec("DELETE FROM leaf WHERE rid=?", rid); err != nil {
			return fmt.Errorf("leafCheck delete rid=%d: %w", rid, err)
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
		if _, err := q.Exec("INSERT OR IGNORE INTO leaf(rid) VALUES(?)", rid); err != nil {
			return fmt.Errorf("leafCheck insert rid=%d: %w", rid, err)
		}
		return nil
	default:
		return fmt.Errorf("leafCheck query rid=%d: %w", rid, err)
	}
}

// repairLeafTable recomputes leaf from scratch. Crosslink does not maintain
// leaf incrementally as each checkin is linked -- that only produces the right
// answer when parents are always linked before their children, which
// delta-chain order does not guarantee -- so the whole table is rebuilt once
// the sweep's plink edges and propagated branch tags are all in place.
//
// This is fossil's leaf_rebuild() with one deliberate difference: fossil draws
// its candidates from `SELECT cid FROM plink`, which cannot return a check-in
// that has no parent, and reaches those through leaf_check() instead. Drawing
// candidates from event covers both in one statement, which is why a repository
// holding a single parentless check-in ends up with the one leaf row fossil
// gives it too.
func repairLeafTable(q db.Querier) error {
	if q == nil {
		panic("manifest.repairLeafTable: q must not be nil")
	}
	if _, err := q.Exec("DELETE FROM leaf"); err != nil {
		return fmt.Errorf("repairLeafTable clear: %w", err)
	}
	if _, err := q.Exec(fmt.Sprintf(`
		INSERT INTO leaf(rid)
		SELECT e.objid FROM event e
		WHERE e.type='ci'
		  AND NOT EXISTS(
		    SELECT 1 FROM plink
		     WHERE plink.pid = e.objid
		       AND coalesce((SELECT value FROM tagxref
		                      WHERE tagid=%d AND rid=plink.pid),'trunk')
		         = coalesce((SELECT value FROM tagxref
		                      WHERE tagid=%d AND rid=plink.cid),'trunk'))
	`, branchTagID, branchTagID)); err != nil {
		return fmt.Errorf("repairLeafTable insert: %w", err)
	}
	return nil
}
