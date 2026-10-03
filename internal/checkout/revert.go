package checkout

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
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

	// Build query based on whether specific paths are requested
	if len(opts.Paths) > 0 {
		// Revert specific paths
		// We'll iterate over each path to avoid complex IN clause handling
		for _, path := range opts.Paths {
			if err := c.revertSinglePath(vid, path, opts.Callback); err != nil {
				return fmt.Errorf("checkout.Revert: %w", err)
			}
		}
		return nil
	}

	// Revert all changed files
	// First, collect all file info to avoid database lock during DELETE operations
	type fileInfo struct {
		id       int64
		pathname string
		rid      int64
	}
	var filesToRevert []fileInfo

	rows, err := c.db.Query(`
		SELECT id, pathname, rid
		FROM vfile
		WHERE vid = ? AND (chnged > 0 OR deleted > 0 OR rid = 0)
	`, int64(vid))
	if err != nil {
		return fmt.Errorf("checkout.Revert: query vfile: %w", err)
	}

	for rows.Next() {
		var fi fileInfo
		if err := rows.Scan(&fi.id, &fi.pathname, &fi.rid); err != nil {
			rows.Close()
			return fmt.Errorf("checkout.Revert: scan vfile row: %w", err)
		}
		filesToRevert = append(filesToRevert, fi)
	}
	rows.Close()

	if err := rows.Err(); err != nil {
		return fmt.Errorf("checkout.Revert: iterate vfile rows: %w", err)
	}

	// Now process each file (safe to DELETE now that query is closed)
	for _, fi := range filesToRevert {
		if err := c.revertFile(fi.id, fi.pathname, fi.rid, opts.Callback); err != nil {
			return fmt.Errorf("checkout.Revert: %w", err)
		}
	}

	return nil
}

// revertSinglePath reverts a specific path.
func (c *Checkout) revertSinglePath(
	vid libfossil.FslID, pathname string,
	callback func(string, RevertChange) error,
) error {
	var id, rid, chnged, deleted int64
	err := c.db.QueryRow(`
		SELECT id, rid, CAST(chnged AS INTEGER), CAST(deleted AS INTEGER)
		FROM vfile
		WHERE vid = ? AND pathname = ?
	`, int64(vid), pathname).Scan(&id, &rid, &chnged, &deleted)

	if err == sql.ErrNoRows {
		// File not tracked, nothing to revert
		return nil
	}
	if err != nil {
		return fmt.Errorf("checkout.Revert: query vfile for %s: %w", pathname, err)
	}

	// Only revert if there are changes
	if chnged == 0 && deleted == 0 && rid != 0 {
		// No changes to revert
		return nil
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
// with the row's executable bit, and marks row id unchanged.
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
	if _, err := c.db.Exec(
		"UPDATE vfile SET chnged = 0, deleted = 0 WHERE id = ?", id,
	); err != nil {
		return fmt.Errorf("checkout.Revert: update vfile for %s: %w", pathname, err)
	}
	return nil
}
