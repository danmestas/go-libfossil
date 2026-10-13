// Package leaf maintains the leaf table, in one place so every writer of a
// branch tag -- crosslinking, a local commit, a tag control artifact -- and the
// explicit rebuild agree on what a leaf is and on when one is rechecked.
//
// It is a package of its own, below both internal/tag and internal/manifest,
// because the tag package is where branch rows are written (and so where a
// recheck must be scheduled), while manifest owns the crosslink batches that
// apply the schedule. Neither can import the other in that direction.
//
// Canonical fossil's rule (src/leaf.c, leaf_check/is_a_leaf/leaf_rebuild): a
// check-in stops being a leaf only once a child of it carries the *same* branch
// name, where a check-in with no active branch tag reads as the default branch
// (here, the main-branch setting, see mainBranchSQL):
//
//	SELECT 1 FROM plink
//	 WHERE pid=:rid
//	   AND coalesce((SELECT value FROM tagxref
//	                  WHERE tagid=TAG_BRANCH AND rid=plink.pid
//	                    AND tagtype>0),'trunk')
//	    == coalesce((SELECT value FROM tagxref
//	                  WHERE tagid=TAG_BRANCH AND rid=plink.cid
//	                    AND tagtype>0),'trunk')
//
// Only an active branch row (tagtype > 0) names a branch. fossil's is_a_leaf
// and leaf_rebuild filter so, but its leaf_check, which maintains the table,
// does not: a check-in whose branch tag was cancelled with a value keeps that
// value there. draft-fossil-repo-state-01 Section 6.4 requires the filter on
// every path that decides branch membership, so both paths here apply it.
//
// Two consequences are easy to get wrong and are what issue #189 was:
//
//   - The merge parent of a check-in on another branch stays a leaf. A branch
//     merged back into trunk keeps its tip in the leaf table -- that class was
//     939 of the 1,426 leaves the real Fossil repository has.
//   - The plink row's isprim flag is not consulted at all. A branch point stays
//     a leaf even though its only child is its *primary* child, because that
//     child is on a different branch.
package leaf

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// BranchTagID is the tagid db/schema.go seeds for 'branch', which is fossil's
// own TAG_BRANCH; both schemas fix it, so it is a constant rather than a
// lookup per row of a whole-repository rebuild.
const BranchTagID = 8

// mainBranchSQL is the default branch a check-in with no active branch row
// reads as: the main-branch setting, or trunk when it is unset. Section 6.1
// forbids assuming "trunk" when the setting is present. fossil's leaf.c uses
// the literal 'trunk' here; the -01 text is followed, as for repo.MainBranch,
// which also reads an empty value as unset.
const mainBranchSQL = `coalesce(nullif((SELECT value FROM config
	                                  WHERE name='main-branch'),''),'trunk')`

// sameBranchSQL is true for a plink row whose child is on its parent's branch,
// each read through its active branch row only.
var sameBranchSQL = fmt.Sprintf(`
	coalesce((SELECT value FROM tagxref
	           WHERE tagid=%[1]d AND rid=plink.pid AND tagtype>0),%[2]s)
	  = coalesce((SELECT value FROM tagxref
	               WHERE tagid=%[1]d AND rid=plink.cid AND tagtype>0),%[2]s)`,
	BranchTagID, mainBranchSQL)

// Check brings rid's leaf row in line with the rule above, inserting or
// deleting as needed: Section 7.3's incremental check, fossil's leaf_check().
// It reads tagxref, so it must run once every branch row it compares is in
// place; Pending.Apply is how the write paths reach it.
func Check(q db.Querier, rid libfossil.FslID) error {
	if q == nil {
		panic("leaf.Check: q must not be nil")
	}
	if rid <= 0 {
		panic("leaf.Check: rid must be positive")
	}

	var one int
	err := q.QueryRow(`
		SELECT 1 FROM plink
		 WHERE plink.pid = ? AND `+sameBranchSQL+`
		 LIMIT 1`, rid).Scan(&one)
	switch {
	case err == nil:
		if _, err := q.Exec("DELETE FROM leaf WHERE rid=?", rid); err != nil {
			return fmt.Errorf("leaf.Check delete rid=%d: %w", rid, err)
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
		if _, err := q.Exec("INSERT OR IGNORE INTO leaf(rid) VALUES(?)", rid); err != nil {
			return fmt.Errorf("leaf.Check insert rid=%d: %w", rid, err)
		}
		return nil
	default:
		return fmt.Errorf("leaf.Check query rid=%d: %w", rid, err)
	}
}

// Rebuild recomputes the whole leaf table: Section 7.3's full rebuild, the
// table crosslinking every check-in in a single batch produces. It scans the
// repository, so only an explicit rebuild calls it (verify.Rebuild through
// manifest.CrosslinkRebuild); every routine write goes through Pending.
//
// The candidates are the rids a single batch would have scheduled: every rid
// carrying a branch tagxref row of any type, plus every rid that is a parent
// in plink. That is NOT "every check-in". A check-in that has a parent but
// carries no branch tag is never checked, so it is never a leaf; the
// corrected Section 7.3 states the same rule, and widening the set to plink
// children changed the leaf table of a real fossil-authored repository (the
// libfossil corpus holds a tagless check-in with a parent), which
// `fossil rebuild` does not reproduce.
//
// Taking every plink parent, rather than only the parents of tagged
// check-ins, adds no member: a parent reached only through untagged children
// is either untagged itself, and so on the default branch with them, or
// tagged and a candidate anyway.
//
// The set still differs from "every check-in" in both directions. A parent
// whose own artifact never arrived has no event row at all, yet fossil does
// list such a phantom as a leaf when the check-in naming it sits on another
// branch. In the other direction, a check-in that neither declares nor
// inherits a branch tag and has no ancestry -- `fossil merge --integrate`
// writes one of these, a comment and a `T +closed` card and nothing else --
// is never checked, and is absent from fossil's leaf table even though the
// rule would admit it. Drawing candidates from event instead put four such
// rows in a corpus clone that `fossil rebuild` then removed (issue #193).
func Rebuild(q db.Querier) error {
	if q == nil {
		panic("leaf.Rebuild: q must not be nil")
	}
	if _, err := q.Exec("DELETE FROM leaf"); err != nil {
		return fmt.Errorf("leaf.Rebuild clear: %w", err)
	}
	if _, err := q.Exec(fmt.Sprintf(`
		INSERT INTO leaf(rid)
		SELECT c.rid FROM (
		    SELECT rid FROM tagxref WHERE tagid=%d
		    UNION
		    SELECT pid AS rid FROM plink WHERE pid > 0
		) c
		WHERE NOT EXISTS(
		    SELECT 1 FROM plink
		     WHERE plink.pid = c.rid AND %s)
	`, BranchTagID, sameBranchSQL)); err != nil {
		return fmt.Errorf("leaf.Rebuild insert: %w", err)
	}
	return nil
}

// Pending is Section 7.3's deferred schedule, fossil's needToCheck bag
// (src/leaf.c leaf_eventually_check, leaf_do_pending_checks): the check-ins a
// batch must recheck before its leaf table is read.
//
// A check-in is scheduled only when a branch tag of any type is written to it
// or the propagation walk updates or deletes its branch row (src/tag.c
// tag_insert and tag_propagate); internal/tag calls Schedule at exactly those
// writes. Crosslinking a check-in or adding a plink row schedules nothing, so
// a check-in keeps its verdict when an untagged child is linked to it in a
// later batch (issue #426).
//
// It is not safe for concurrent use; one batch owns one.
type Pending struct {
	// direct are the rids a branch write scheduled, in scheduling order.
	direct []libfossil.FslID
	// checks are the rids Apply checks: each direct rid and every parent it
	// had, in plink, when it was scheduled. order lists them as they were
	// added so Rollback can drop exactly what a failed transaction added.
	checks map[libfossil.FslID]struct{}
	order  []libfossil.FslID
}

// NewPending returns an empty schedule.
func NewPending() *Pending {
	return &Pending{checks: make(map[libfossil.FslID]struct{})}
}

// Mark is a point a schedule can be rolled back to; see Pending.Mark.
type Mark struct{ direct, order int }

// Schedule queues rid and each of its current plink parents, merge parents
// included, for a recheck. It is leaf_eventually_check(): fossil reads the
// parents here, at the branch write, so a parent the batch later loses (a
// reparent) is still checked; Apply adds the parents rid has by then.
func (p *Pending) Schedule(q db.Querier, rid libfossil.FslID) error {
	if p == nil {
		panic("leaf.Pending.Schedule: p must not be nil")
	}
	if q == nil {
		panic("leaf.Pending.Schedule: q must not be nil")
	}
	if rid <= 0 {
		panic("leaf.Pending.Schedule: rid must be positive")
	}
	p.direct = append(p.direct, rid)
	p.add(rid)
	parents, err := parentsOf(q, rid)
	if err != nil {
		return fmt.Errorf("leaf.Pending.Schedule rid=%d: %w", rid, err)
	}
	for _, pid := range parents {
		p.add(pid)
	}
	return nil
}

func (p *Pending) add(rid libfossil.FslID) {
	if _, ok := p.checks[rid]; ok {
		return
	}
	p.checks[rid] = struct{}{}
	p.order = append(p.order, rid)
}

// Len reports how many check-ins the schedule would check now, before Apply
// adds the parents scheduled check-ins have gained since.
func (p *Pending) Len() int {
	if p == nil {
		panic("leaf.Pending.Len: p must not be nil")
	}
	return len(p.checks)
}

// Mark records the schedule's current extent. A caller whose transaction or
// savepoint rolls back passes it to Rollback, so the branch writes that rolled
// back do not leave rechecks behind for check-ins the rule never reached.
func (p *Pending) Mark() Mark {
	if p == nil {
		panic("leaf.Pending.Mark: p must not be nil")
	}
	return Mark{direct: len(p.direct), order: len(p.order)}
}

// Rollback forgets everything scheduled since m.
func (p *Pending) Rollback(m Mark) {
	if p == nil {
		panic("leaf.Pending.Rollback: p must not be nil")
	}
	if m.direct > len(p.direct) || m.order > len(p.order) {
		panic("leaf.Pending.Rollback: mark is past the schedule's end")
	}
	for _, rid := range p.order[m.order:] {
		delete(p.checks, rid)
	}
	p.direct = p.direct[:m.direct]
	p.order = p.order[:m.order]
}

// Apply runs the incremental check on every scheduled check-in and on its
// parents, those it had when scheduled and those it has now, and keeps the
// schedule: the caller clears it with Reset once the transaction holding the
// checks has committed, or rolls it back if it did not. The work is bounded by
// what the batch scheduled, never by the repository.
func (p *Pending) Apply(q db.Querier) error {
	if p == nil {
		panic("leaf.Pending.Apply: p must not be nil")
	}
	if q == nil {
		panic("leaf.Pending.Apply: q must not be nil")
	}
	todo := make(map[libfossil.FslID]struct{}, len(p.checks))
	for rid := range p.checks {
		todo[rid] = struct{}{}
	}
	for _, rid := range p.direct {
		parents, err := parentsOf(q, rid)
		if err != nil {
			return fmt.Errorf("leaf.Pending.Apply rid=%d: %w", rid, err)
		}
		for _, pid := range parents {
			todo[pid] = struct{}{}
		}
	}
	// Each check reads plink and tagxref and writes only its own leaf row, so
	// the order cannot change the result; sorting keeps the writes
	// reproducible run to run.
	rids := make([]libfossil.FslID, 0, len(todo))
	for rid := range todo {
		rids = append(rids, rid)
	}
	sort.Slice(rids, func(i, j int) bool { return rids[i] < rids[j] })
	for _, rid := range rids {
		if err := Check(q, rid); err != nil {
			return err
		}
	}
	return nil
}

// Reset empties the schedule, as leaf_do_pending_checks does once it has run.
func (p *Pending) Reset() {
	if p == nil {
		panic("leaf.Pending.Reset: p must not be nil")
	}
	p.direct = p.direct[:0]
	p.order = p.order[:0]
	clear(p.checks)
}

// parentsOf lists rid's plink parents, merge parents included.
func parentsOf(q db.Querier, rid libfossil.FslID) ([]libfossil.FslID, error) {
	rows, err := q.Query("SELECT pid FROM plink WHERE cid=? AND pid>0", rid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []libfossil.FslID
	for rows.Next() {
		var pid int64
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		out = append(out, libfossil.FslID(pid))
	}
	return out, rows.Err()
}
