package checkout

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// CalcUpdateVersion finds the check-in an update with no target moves to,
// as fossil's update does when no VERSION is named (src/update.c): the
// newest open leaf descended from the checkout (every leaf when all are
// closed), kept to the checkout's branch when there are several. If several
// remain on that branch the target is underdetermined and an error is
// returned, where fossil stops with "Multiple descendants". Returns 0 when
// the checkout is already there or nothing descends from it.
//
// Departures from fossil 2.28, for repo-state-00: the checkout's branch and
// the same-branch tests read only active branch rows (Section 6.4), and a
// check-in with no active branch row is on the main-branch setting's branch
// rather than the literal trunk (Section 6.1), for the checkout and for the
// leaves alike. So an untagged checkout is kept to the default branch,
// where fossil compares its missing row, keeps no leaf and stays put.
//
// Section 10.3 says closed-ness and leaf-ness are not inputs to update's
// target computation. That is read as covering a named update ("update
// trunk"); fossil's update with no version named drops closed leaves and is
// built on the leaf set, and -00 does not define that form. Erratum
// candidate.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) CalcUpdateVersion() (libfossil.FslID, error) {
	if c == nil {
		panic("checkout.CalcUpdateVersion: nil *Checkout")
	}
	vid, _, err := c.Version()
	if err != nil {
		return 0, fmt.Errorf("checkout.CalcUpdateVersion: %w", err)
	}
	leaves, err := descendantLeaves(c.repo, vid, true)
	if err == nil && len(leaves) == 0 {
		leaves, err = descendantLeaves(c.repo, vid, false)
	}
	if err != nil {
		return 0, fmt.Errorf("checkout.CalcUpdateVersion: %w", err)
	}
	if len(leaves) > 1 {
		// fossil keeps the leaves on the checkout's branch; a checkout with
		// no version has none, so none are kept and it stays put.
		branch := ""
		if vid > 0 {
			if branch, err = branchOf(c.repo, vid); err != nil {
				return 0, fmt.Errorf("checkout.CalcUpdateVersion: %w", err)
			}
		}
		if leaves, err = keepBranch(c.repo, leaves, branch); err != nil {
			return 0, fmt.Errorf("checkout.CalcUpdateVersion: %w", err)
		}
		if len(leaves) > 1 {
			return 0, fmt.Errorf("checkout.CalcUpdateVersion: multiple descendants: "+
				"%d leaves on branch %q descend from the checkout; name a target",
				len(leaves), branch)
		}
	}
	tid, err := newestCheckin(c.repo, leaves)
	if err != nil {
		return 0, fmt.Errorf("checkout.CalcUpdateVersion: %w", err)
	}
	if tid == vid {
		return 0, nil
	}
	return tid, nil
}

// closedSQL is true when the check-in %s carries an active closed tag,
// fossil's leaf_is_closed_sql (src/leaf.c).
const closedSQL = `EXISTS(SELECT 1 FROM tagxref cx JOIN tag ct ON ct.tagid = cx.tagid
	WHERE ct.tagname = 'closed' AND cx.rid = %s AND cx.tagtype > 0)`

// branchStartSQL is true when plink's child starts a branch: it declares a
// propagating branch tag itself (fossil's isBr query in compute_leaves).
const branchStartSQL = `EXISTS(SELECT 1 FROM tagxref bx JOIN tag bt ON bt.tagid = bx.tagid
	WHERE bt.tagname = 'branch' AND bx.rid = plink.cid
	  AND bx.tagtype = 2 AND bx.srcid > 0)`

// descendantLeaves is fossil's compute_leaves(vid, closeMode) (src/
// descendants.c): the leaves reached from vid by primary links and by
// same-branch merge links. A check-in is one of them when no child of it is
// on its branch and none is a primary child that does not start a branch.
// openOnly drops closed leaves. A vid of 0 means every leaf in the table.
func descendantLeaves(
	r *repo.Repo, vid libfossil.FslID, openOnly bool,
) ([]libfossil.FslID, error) {
	if r == nil {
		panic("checkout.descendantLeaves: r must not be nil")
	}
	if vid < 0 {
		panic("checkout.descendantLeaves: vid must not be negative")
	}
	open := ""
	if openOnly {
		open = " AND NOT " + fmt.Sprintf(closedSQL, "d.rid")
	}
	if vid == 0 {
		return queryRids(r, `SELECT d.rid FROM leaf d WHERE 1`+open)
	}
	return queryRids(r, `
		WITH RECURSIVE d(rid) AS (
		  SELECT ?
		  UNION
		  SELECT plink.cid FROM plink JOIN d ON plink.pid = d.rid
		   WHERE plink.isprim OR `+sameBranchSQL+`
		)
		SELECT d.rid FROM d
		 WHERE NOT EXISTS (
		   SELECT 1 FROM plink
		    WHERE plink.pid = d.rid
		      AND (`+sameBranchSQL+`
		           OR (plink.isprim AND NOT `+branchStartSQL+`)))`+open, int64(vid))
}

// keepBranch returns the leaves on branch, each read through its active
// branch row and through the main-branch setting when it has none, the same
// reading sameBranchSQL and the checkout's own branch use (Section 6.1).
func keepBranch(
	r *repo.Repo, leaves []libfossil.FslID, branch string,
) ([]libfossil.FslID, error) {
	if r == nil {
		panic("checkout.keepBranch: r must not be nil")
	}
	var kept []libfossil.FslID
	for _, rid := range leaves {
		var one int
		err := r.DB().QueryRow(`
			SELECT 1 WHERE coalesce((SELECT tx.value FROM tagxref tx
			   JOIN tag t ON t.tagid = tx.tagid
			   WHERE t.tagname = 'branch' AND tx.rid = ? AND tx.tagtype > 0),
			   `+mainBranchSQL+`) = ?`, int64(rid), branch).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("keepBranch: %w", err)
		}
		kept = append(kept, rid)
	}
	return kept, nil
}

// newestCheckin returns the rid among rids with the greatest event mtime, 0
// when none has an event row. The comparison stays in SQL: a driver may scan
// the mtime column as a time rather than the julian day it stores.
func newestCheckin(r *repo.Repo, rids []libfossil.FslID) (libfossil.FslID, error) {
	if r == nil {
		panic("checkout.newestCheckin: r must not be nil")
	}
	if len(rids) == 0 {
		return 0, nil
	}
	marks := strings.Repeat(",?", len(rids))[1:]
	args := make([]any, len(rids))
	for i, rid := range rids {
		args[i] = int64(rid)
	}
	var best int64
	err := r.DB().QueryRow(`SELECT objid FROM event WHERE objid IN (`+marks+`)
		ORDER BY mtime DESC LIMIT 1`, args...).Scan(&best)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("newestCheckin: %w", err)
	}
	return libfossil.FslID(best), nil
}

// queryRids runs query with args and collects its single rid column.
func queryRids(r *repo.Repo, query string, args ...any) ([]libfossil.FslID, error) {
	if r == nil {
		panic("checkout.queryRids: r must not be nil")
	}
	rows, err := r.DB().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rids []libfossil.FslID
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return nil, err
		}
		rids = append(rids, libfossil.FslID(rid))
	}
	return rids, rows.Err()
}
