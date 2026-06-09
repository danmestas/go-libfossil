package checkout

import (
	"database/sql"
	"fmt"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
)

// HasChanges returns true if the checkout has any modified, deleted, or renamed files.
// This is a DB-only check that does NOT scan the filesystem.
// Use ScanChanges first if you need to detect on-disk modifications.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) HasChanges() (bool, error) {
	if c == nil {
		panic("checkout.HasChanges: nil *Checkout")
	}

	// Get current checkout version
	rid, _, err := c.Version()
	if err != nil {
		return false, fmt.Errorf("checkout.HasChanges: %w", err)
	}

	// Check for any changed or deleted files
	var count int
	err = c.db.QueryRow(`
		SELECT count(*) FROM vfile
		WHERE (chnged > 0 OR deleted > 0) AND vid = ?
	`, int64(rid)).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checkout.HasChanges: query: %w", err)
	}

	return count > 0, nil
}

// VisitChanges iterates over all changed files in the checkout, calling fn for each.
// If scan=true, calls ScanChanges(ScanHash) first to detect on-disk modifications.
// Otherwise, only reports files marked as changed in the vfile table.
//
// The function classifies each changed file:
// - deleted > 0 → ChangeRemoved
// - rid == 0 → ChangeAdded (newly managed file, not yet committed)
// - origname != "" → ChangeRenamed
// - chnged > 0 → ChangeModified
//
// If fn returns a non-nil error, iteration stops and that error is returned.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) VisitChanges(vid libfossil.FslID, scan bool, fn ChangeVisitor) error {
	if c == nil {
		panic("checkout.VisitChanges: nil *Checkout")
	}

	// Scan filesystem if requested
	if scan {
		if err := c.ScanChanges(ScanHash); err != nil {
			return fmt.Errorf("checkout.VisitChanges: %w", err)
		}
	}

	// Query all changed/deleted/renamed files
	rows, err := c.db.Query(`
		SELECT id, pathname, CAST(chnged AS INTEGER), CAST(deleted AS INTEGER),
		       CAST(isexe AS INTEGER), CAST(islink AS INTEGER), origname, rid
		FROM vfile
		WHERE vid = ? AND (chnged > 0 OR deleted > 0 OR origname IS NOT NULL)
	`, int64(vid))
	if err != nil {
		return fmt.Errorf("checkout.VisitChanges: query: %w", err)
	}
	defer rows.Close()

	// Iterate over each changed file
	for rows.Next() {
		var id, chnged, deleted, isexe, islink, rid int64
		var pathname string
		var origname sql.NullString

		if err := rows.Scan(
			&id, &pathname, &chnged, &deleted,
			&isexe, &islink, &origname, &rid,
		); err != nil {
			return fmt.Errorf("checkout.VisitChanges: scan: %w", err)
		}

		// Determine change type (priority order: deleted > added > renamed > modified)
		var changeType FileChange
		if deleted > 0 {
			changeType = ChangeRemoved
		} else if rid == 0 {
			changeType = ChangeAdded
		} else if origname.Valid && origname.String != "" {
			changeType = ChangeRenamed
		} else if chnged > 0 {
			changeType = ChangeModified
		} else {
			// Should not reach here given WHERE clause, but handle gracefully
			changeType = ChangeNone
		}

		// Build change entry
		entry := ChangeEntry{
			Name:     pathname,
			Change:   changeType,
			VFileID:  libfossil.FslID(id),
			IsExec:   isexe != 0,
			IsLink:   islink != 0,
			OrigName: origname.String,
		}

		// Call visitor function
		if err := fn(entry); err != nil {
			return fmt.Errorf("checkout.VisitChanges: visitor for %s: %w", pathname, err)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("checkout.VisitChanges: iterate: %w", err)
	}

	return nil
}
