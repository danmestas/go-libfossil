package checkout

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// Revert restores files to their checkout version state, the way fossil's
// revert does. If opts.Paths is empty, reverts ALL changed files. Change
// flags are refreshed from disk first, so an edit not yet scanned is
// reverted too.
//
// For each file to revert:
//   - If rid==0 (newly added): un-manage it (DELETE from vfile). The file stays
//     on disk: it was never committed, so deleting it would destroy the only copy.
//   - If rid>0 (existing, modified/deleted): restore original content, reset chnged=0, deleted=0
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Revert(opts RevertOpts) error {
	if c == nil {
		panic("checkout.Revert: nil *Checkout")
	}

	// Get current version
	vid, _, err := c.Version()
	if err != nil {
		return fmt.Errorf("checkout.Revert: %w", err)
	}
	if vid > 0 {
		if err := c.refreshChanged(vid); err != nil {
			return fmt.Errorf("checkout.Revert: %w", err)
		}
	}

	if len(opts.Paths) > 0 {
		for _, path := range opts.Paths {
			if err := c.revertSinglePath(vid, path, opts.Callback); err != nil {
				return fmt.Errorf("checkout.Revert: %w", err)
			}
		}
		return nil
	}
	if err := c.revertAll(vid, opts.Callback); err != nil {
		return fmt.Errorf("checkout.Revert: %w", err)
	}
	return nil
}

// revertAll reverts every changed file of version vid and drops any pending
// merge, as fossil's revert does with no file named. A file is reverted when
// its content changed, it is removed, an add is pending, a merge touched it,
// or it is missing from disk. A pure rename is left for #242: revertFile
// cannot move a file back to its old name yet.
func (c *Checkout) revertAll(vid libfossil.FslID, callback func(string, RevertChange) error) error {
	if vid < 0 {
		panic("checkout.revertAll: negative vid")
	}

	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil {
		return err
	}
	for _, r := range rows {
		missing, err := c.isMissing(r)
		if err != nil {
			return err
		}
		if !needsRevert(r, missing) {
			continue
		}
		if err := c.revertFile(r.ID, r.Pathname, r.RID, callback); err != nil {
			return err
		}
	}
	if _, err := c.db.Exec("DELETE FROM vmerge"); err != nil {
		return fmt.Errorf("clear vmerge: %w", err)
	}
	return nil
}

// needsRevert reports whether revert-all restores row r: its content
// changed, it is removed, a merge touched it, or it is missing from disk.
func needsRevert(r vfile.Row, missing bool) bool {
	if missing {
		return true
	}
	if r.ContentChanged() {
		return true
	}
	if r.IsRemoved() {
		return true
	}
	return r.PendingMerge()
}

// revertSinglePath reverts a named file whatever its change flags say, as
// fossil's revert does for a file it is given: a file missing from disk is
// restored too. A path the checkout does not track is a no-op, as in fossil.
func (c *Checkout) revertSinglePath(
	vid libfossil.FslID, pathname string,
	callback func(string, RevertChange) error,
) error {
	var id, rid int64
	err := c.db.QueryRow(
		"SELECT id, rid FROM vfile WHERE vid = ? AND pathname = ?", int64(vid), pathname,
	).Scan(&id, &rid)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checkout.Revert: query vfile for %s: %w", pathname, err)
	}
	return c.revertFile(id, pathname, rid, callback)
}

// revertFile handles the revert logic for a single file.
func (c *Checkout) revertFile(
	id int64, pathname string, rid int64,
	callback func(string, RevertChange) error,
) error {
	fullPath, err := c.safePath(pathname)
	if err != nil {
		return fmt.Errorf("checkout.Revert: path traversal in %s: %w", pathname, err)
	}

	if rid == 0 {
		// Newly added file (never committed): un-manage it and leave it on
		// disk, as fossil's revert does. It has no committed copy to restore.
		_, err := c.db.Exec("DELETE FROM vfile WHERE id = ?", id)
		if err != nil {
			return fmt.Errorf("checkout.Revert: delete vfile for %s: %w", pathname, err)
		}

		// Notify callback
		if callback != nil {
			if err := callback(pathname, RevertUnmanage); err != nil {
				return fmt.Errorf("checkout.Revert: callback for %s: %w", pathname, err)
			}
		}

		return nil
	}

	if err := c.restoreCommitted(id, pathname, fullPath, rid); err != nil {
		return err
	}

	// Notify callback
	if callback != nil {
		if err := callback(pathname, RevertContents); err != nil {
			return fmt.Errorf("checkout.Revert: callback for %s: %w", pathname, err)
		}
	}

	return nil
}

// restoreCommitted writes the committed content of blob rid back to fullPath,
// with the row's executable bit, and marks row id unchanged and unmerged.
func (c *Checkout) restoreCommitted(id int64, pathname, fullPath string, rid int64) error {
	if rid <= 0 {
		panic("checkout.restoreCommitted: rid must be positive")
	}
	if fullPath == "" {
		panic("checkout.restoreCommitted: empty fullPath")
	}

	data, err := content.Expand(c.repo.DB(), libfossil.FslID(rid))
	if err != nil {
		return fmt.Errorf("checkout.Revert: expand blob for %s: %w", pathname, err)
	}
	var isexe int64
	err = c.db.QueryRow("SELECT CAST(isexe AS INTEGER) FROM vfile WHERE id = ?", id).Scan(&isexe)
	if err != nil {
		return fmt.Errorf("checkout.Revert: query vfile metadata for %s: %w", pathname, err)
	}
	parentDir := filepath.Dir(fullPath)
	if err := c.env.Storage.MkdirAll(parentDir, 0o755); err != nil {
		return fmt.Errorf("checkout.Revert: mkdir %s: %w", parentDir, err)
	}
	perm := os.FileMode(0o644)
	if isexe != 0 {
		perm = 0o755
	}
	if err := c.env.Storage.WriteFile(fullPath, data, perm); err != nil {
		return fmt.Errorf("checkout.Revert: write %s: %w", fullPath, err)
	}
	// mrid and mhash go back to the committed version, so no merged-in
	// content outlives the revert (fossil resets them the same way).
	if _, err := c.db.Exec(
		"UPDATE vfile SET chnged = 0, deleted = 0, mrid = rid, mhash = NULL WHERE id = ?", id,
	); err != nil {
		return fmt.Errorf("checkout.Revert: update vfile for %s: %w", pathname, err)
	}
	return nil
}
