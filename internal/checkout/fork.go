package checkout

import (
	"fmt"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// BranchLeaves returns the leaf RIDs for a named branch.
// A branch with >1 leaf is forked. An empty branch name queries trunk.
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

// WouldFork reports whether committing on the current branch would
// create a fork. Returns true when another leaf exists on the same
// branch that is not the current checkout version.
func (c *Checkout) WouldFork() (bool, error) {
	if c == nil {
		panic("checkout.WouldFork: nil *Checkout")
	}
	rid, _, err := c.Version()
	if err != nil {
		return false, fmt.Errorf("checkout.WouldFork: %w", err)
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

// currentBranch returns the branch name for the given RID.
// Falls back to "trunk" if no branch tag exists.
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
	if err != nil {
		return "trunk", nil // no branch tag → trunk
	}
	return branch, nil
}
