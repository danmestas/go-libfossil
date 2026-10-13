package checkout

import (
	"database/sql"
	"errors"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// BranchLeaves returns the open leaf RIDs for a named branch: a leaf whose
// active branch tag is branch and that carries no 'closed' tag, as fossil's
// `leaves` lists them (src/leaf.c leaf_is_closed_sql). A branch with >1 open
// leaf is forked. An empty branch name queries the default branch, the
// main-branch setting (repo.MainBranch).
func BranchLeaves(r *repo.Repo, branch string) ([]libfossil.FslID, error) {
	if r == nil {
		panic("checkout.BranchLeaves: r must not be nil")
	}
	if branch == "" {
		name, err := repo.MainBranch(r.DB())
		if err != nil {
			return nil, fmt.Errorf("checkout.BranchLeaves: %w", err)
		}
		branch = name
	}
	rows, err := r.DB().Query(`
		SELECT l.rid FROM leaf l
		JOIN tagxref tx ON tx.rid = l.rid
		JOIN tag t ON t.tagid = tx.tagid
		WHERE t.tagname = 'branch'
		  AND tx.value = ?
		  AND tx.tagtype > 0
		  AND NOT EXISTS (
		    SELECT 1 FROM tagxref cx
		    JOIN tag ct ON ct.tagid = cx.tagid
		    WHERE ct.tagname = 'closed'
		      AND cx.rid = l.rid
		      AND cx.tagtype > 0
		  )
	`, branch)
	if err != nil {
		return nil, fmt.Errorf("checkout.BranchLeaves: %w", err)
	}
	defer rows.Close()
	var leaves []libfossil.FslID
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return nil, fmt.Errorf("checkout.BranchLeaves scan: %w", err)
		}
		leaves = append(leaves, libfossil.FslID(rid))
	}
	return leaves, rows.Err()
}

// WouldFork reports whether committing on the checkout's version would
// create a fork: the version already has a child on its own branch, so a new
// check-in would become a second leaf beside it. This is fossil's commit-time
// refusal (src/checkin.c "Would fork", taken when !is_a_leaf(vid)). Another
// open leaf elsewhere on the branch does not make a commit fork -- the commit
// extends this leaf -- and is reported by LeafAmbiguous instead. A checkout
// with no version (an empty repository) cannot fork.
func (c *Checkout) WouldFork() (bool, error) {
	if c == nil {
		panic("checkout.WouldFork: nil *Checkout")
	}
	rid, _, err := c.Version()
	if err != nil {
		return false, fmt.Errorf("checkout.WouldFork: %w", err)
	}
	if rid <= 0 {
		return false, nil
	}
	return hasSameBranchChild(c.repo, rid)
}

// hasSameBranchChild reports whether rid has a child check-in on the same
// branch, computed from plink as fossil's is_a_leaf (src/leaf.c) does, with
// no active branch tag meaning the main branch (Section 6.1: the
// main-branch setting, trunk when unset). Reading plink rather than the leaf table keeps
// the commit check correct even if the leaf table has drifted.
func hasSameBranchChild(r *repo.Repo, rid libfossil.FslID) (bool, error) {
	if rid <= 0 {
		panic("checkout.hasSameBranchChild: rid must be positive")
	}
	var one int
	err := r.DB().QueryRow(`
		SELECT 1 FROM plink
		WHERE pid = ? AND `+sameBranchSQL+`
		LIMIT 1
	`, int64(rid)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checkout.hasSameBranchChild: %w", err)
	}
	return true, nil
}

// mainBranchSQL is repo.MainBranch as an expression, for use inside a query.
const mainBranchSQL = `coalesce(nullif((SELECT value FROM config
		                          WHERE name = 'main-branch'), ''), 'trunk')`

// sameBranchSQL is true for a plink row whose child is on its parent's
// branch, each read through its active branch row only (Section 6.4) and
// through mainBranchSQL when it has none (Section 6.1).
const sameBranchSQL = `coalesce((SELECT tx.value FROM tagxref tx
	                JOIN tag t ON t.tagid = tx.tagid
	                WHERE t.tagname = 'branch' AND tx.rid = plink.pid
	                  AND tx.tagtype > 0), ` + mainBranchSQL + `)
	    = coalesce((SELECT tx.value FROM tagxref tx
	                JOIN tag t ON t.tagid = tx.tagid
	                WHERE t.tagname = 'branch' AND tx.rid = plink.cid
	                  AND tx.tagtype > 0), ` + mainBranchSQL + `)`

// LeafAmbiguous is fossil's leaf_ambiguity (src/leaf.c) and the repo-state
// draft's Section 7.5 ambiguity test: rid must itself be an open leaf, and
// another open leaf must share its branch. It answers whether propagating
// from rid is underdetermined, not whether a commit on rid would fork (see
// WouldFork).
func LeafAmbiguous(r *repo.Repo, rid libfossil.FslID) (bool, error) {
	if r == nil {
		panic("checkout.LeafAmbiguous: r must not be nil")
	}
	if rid <= 0 {
		panic("checkout.LeafAmbiguous: rid must be positive")
	}
	open, err := isOpenLeaf(r, rid)
	if err != nil {
		return false, err
	}
	if !open {
		return false, nil
	}
	// repo-state-01 Section 6.1 attributes a check-in with no branch tag to
	// the default branch but says that does not make it an open leaf of that
	// branch, so there is no branch to share and nothing to be ambiguous
	// with (fossil's leaf_ambiguity compares the raw value, NULL matches
	// nothing).
	branch, tagged, err := activeBranch(r, rid)
	if err != nil {
		return false, err
	}
	if !tagged {
		return false, nil
	}
	leaves, err := BranchLeaves(r, branch)
	if err != nil {
		return false, err
	}
	for _, leaf := range leaves {
		if leaf != rid {
			return true, nil
		}
	}
	return false, nil
}

// isOpenLeaf reports whether rid is in the leaf table and carries no 'closed'
// tag -- fossil's is_a_leaf(rid) AND NOT leaf_is_closed(rid), the precondition
// LeafAmbiguous checks before looking for a sibling.
func isOpenLeaf(r *repo.Repo, rid libfossil.FslID) (bool, error) {
	var one int
	err := r.DB().QueryRow(`
		SELECT 1 FROM leaf
		WHERE rid = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM tagxref cx
		    JOIN tag ct ON ct.tagid = cx.tagid
		    WHERE ct.tagname = 'closed'
		      AND cx.rid = leaf.rid
		      AND cx.tagtype > 0
		  )
	`, int64(rid)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checkout.isOpenLeaf: %w", err)
	}
	return true, nil
}

// branchOf returns the branch name for the given RID, the default branch
// (repo.MainBranch) when the check-in carries no active branch tag. A real
// query error is returned rather than masked as the default, so LeafAmbiguous
// surfaces it.
func branchOf(r *repo.Repo, rid libfossil.FslID) (string, error) {
	branch, tagged, err := activeBranch(r, rid)
	if err != nil {
		return "", err
	}
	if !tagged {
		return repo.MainBranch(r.DB())
	}
	return branch, nil
}

// activeBranch returns the value of rid's active branch tag; tagged is false
// when it has none.
func activeBranch(r *repo.Repo, rid libfossil.FslID) (branch string, tagged bool, err error) {
	if r == nil {
		panic("checkout.activeBranch: r must not be nil")
	}
	err = r.DB().QueryRow(`
		SELECT tx.value FROM tagxref tx
		JOIN tag t ON t.tagid = tx.tagid
		WHERE t.tagname = 'branch'
		  AND tx.rid = ?
		  AND tx.tagtype > 0
		ORDER BY tx.mtime DESC
		LIMIT 1
	`, int64(rid)).Scan(&branch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("checkout.branchOf: %w", err)
	}
	return branch, true, nil
}
