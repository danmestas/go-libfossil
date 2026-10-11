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
// leaf is forked. An empty branch name queries trunk.
func BranchLeaves(r *repo.Repo, branch string) ([]libfossil.FslID, error) {
	if r == nil {
		panic("checkout.BranchLeaves: r must not be nil")
	}
	if branch == "" {
		branch = "trunk"
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

// WouldFork reports whether committing on the current branch would create a
// fork. It is fossil's leaf_ambiguity (src/leaf.c): the current version must
// itself be an open leaf, and another open leaf must exist on the same
// branch. A version that is not a leaf (already has a same-branch child), or
// a closed one, cannot fork by a further commit, so both return false.
func (c *Checkout) WouldFork() (bool, error) {
	if c == nil {
		panic("checkout.WouldFork: nil *Checkout")
	}
	rid, _, err := c.Version()
	if err != nil {
		return false, fmt.Errorf("checkout.WouldFork: %w", err)
	}

	open, err := isOpenLeaf(c.repo, rid)
	if err != nil {
		return false, err
	}
	if !open {
		return false, nil
	}

	branch, err := c.currentBranch(rid)
	if err != nil {
		return false, err
	}

	leaves, err := BranchLeaves(c.repo, branch)
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
// leaf_ambiguity checks before looking for a sibling.
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

// currentBranch returns the branch name for the given RID, "trunk" when the
// check-in carries no branch tag. A real query error is returned rather than
// masked as "trunk", so the WouldFork gate above surfaces it.
func (c *Checkout) currentBranch(rid libfossil.FslID) (string, error) {
	var branch string
	err := c.repo.DB().QueryRow(`
		SELECT tx.value FROM tagxref tx
		JOIN tag t ON t.tagid = tx.tagid
		WHERE t.tagname = 'branch'
		  AND tx.rid = ?
		  AND tx.tagtype > 0
		ORDER BY tx.mtime DESC
		LIMIT 1
	`, int64(rid)).Scan(&branch)
	if errors.Is(err, sql.ErrNoRows) {
		return "trunk", nil // no branch tag → trunk
	}
	if err != nil {
		return "", fmt.Errorf("checkout.currentBranch: %w", err)
	}
	return branch, nil
}
