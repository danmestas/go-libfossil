package checkout

import (
	"database/sql"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// Manage adds files to tracking. For each path:
// - Checks if already in vfile (skips if present)
// - Reads file content from Storage
// - Computes hash
// - Inserts into vfile with rid=0, chnged=1 (marks as newly added)
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Manage(opts ManageOpts) (*ManageCounts, error) {
	if c == nil {
		panic("checkout.Manage: nil *Checkout")
	}

	counts := &ManageCounts{}
	vid, _, err := c.Version()
	if err != nil {
		return nil, fmt.Errorf("checkout.Manage: %w", err)
	}

	for _, path := range opts.Paths {
		// Check if already tracked
		var existingID int64
		err := c.db.QueryRow(
			"SELECT id FROM vfile WHERE vid=? AND pathname=?",
			int64(vid), path,
		).Scan(&existingID)
		if err != nil && err != sql.ErrNoRows {
			return counts, fmt.Errorf("checkout.Manage: query vfile: %w", err)
		}
		if err == nil {
			// Already tracked
			counts.Skipped++
			if opts.Callback != nil {
				if err := opts.Callback(path, false); err != nil {
					return counts, fmt.Errorf("checkout.Manage: callback for %s: %w", path, err)
				}
			}
			continue
		}

		// Read file from Storage
		fullPath, err := c.safePath(path)
		if err != nil {
			return counts, fmt.Errorf("checkout.Manage: path traversal in %s: %w", path, err)
		}
		data, err := c.env.Storage.ReadFile(fullPath)
		if err != nil {
			return counts, fmt.Errorf("checkout.Manage: read %s: %w", path, err)
		}

		// Stat the on-disk file to capture its executable bit at add time.
		info, err := c.env.Storage.Stat(fullPath)
		if err != nil {
			return counts, fmt.Errorf("checkout.Manage: stat %s: %w", path, err)
		}
		isexe := 0
		if modeIsExecutable(info.Mode()) {
			isexe = 1
		}

		// Detect repo hash mode (SHA3 for 64-char UUIDs, SHA1 otherwise).
		mhash := hash.SHA1(data)
		var sampleUUID string
		_ = c.repo.DB().QueryRow(
			"SELECT uuid FROM blob WHERE size >= 0 LIMIT 1",
		).Scan(&sampleUUID)
		if len(sampleUUID) > 40 {
			mhash = hash.SHA3(data)
		}

		// Insert into vfile with rid=0 (newly added), chnged=1 (modified)
		_, err = c.db.Exec(
			"INSERT INTO vfile(vid, pathname, rid, mrid, mhash, isexe, chnged) VALUES(?, ?, 0, 0, ?, ?, 1)",
			int64(vid), path, mhash, isexe,
		)
		if err != nil {
			return counts, fmt.Errorf("checkout.Manage: insert vfile: %w", err)
		}

		counts.Added++
		if opts.Callback != nil {
			if err := opts.Callback(path, true); err != nil {
				return counts, fmt.Errorf("checkout.Manage: callback for %s: %w", path, err)
			}
		}
	}

	return counts, nil
}

// Unmanage removes files from tracking. For each path:
// - If rid=0 (newly added, never committed): DELETE from vfile
// - If rid>0 (existing tracked file): UPDATE vfile SET deleted=1
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Unmanage(opts UnmanageOpts) error {
	if c == nil {
		panic("checkout.Unmanage: nil *Checkout")
	}

	vid, _, err := c.Version()
	if err != nil {
		return fmt.Errorf("checkout.Unmanage: %w", err)
	}

	// Handle VFileIDs if provided
	if len(opts.VFileIDs) > 0 {
		for _, vfileID := range opts.VFileIDs {
			var rid int64
			var pathname string
			err := c.db.QueryRow(
				"SELECT rid, pathname FROM vfile WHERE id=?",
				int64(vfileID),
			).Scan(&rid, &pathname)
			if err == sql.ErrNoRows {
				continue // ID not found, skip
			}
			if err != nil {
				return fmt.Errorf("checkout.Unmanage: query vfile id=%d: %w", vfileID, err)
			}

			if err := c.unmanageFile(vfileID, rid, pathname, opts.Callback); err != nil {
				return fmt.Errorf("checkout.Unmanage: %w", err)
			}
		}
		return nil
	}

	// Handle Paths
	for _, path := range opts.Paths {
		var vfileID libfossil.FslID
		var rid int64
		err := c.db.QueryRow(
			"SELECT id, rid FROM vfile WHERE vid=? AND pathname=?",
			int64(vid), path,
		).Scan(&vfileID, &rid)
		if err == sql.ErrNoRows {
			// Not tracked, skip silently
			continue
		}
		if err != nil {
			return fmt.Errorf("checkout.Unmanage: query vfile: %w", err)
		}

		if err := c.unmanageFile(vfileID, rid, path, opts.Callback); err != nil {
			return fmt.Errorf("checkout.Unmanage: %w", err)
		}
	}

	return nil
}

// unmanageFile handles the logic for a single file:
// - rid=0: DELETE (newly added, never committed)
// - rid>0: UPDATE deleted=1 (existing file from repo)
func (c *Checkout) unmanageFile(
	vfileID libfossil.FslID, rid int64,
	pathname string, callback func(string) error,
) error {
	if rid == 0 {
		// Newly added, never committed — remove completely
		_, err := c.db.Exec("DELETE FROM vfile WHERE id=?", int64(vfileID))
		if err != nil {
			return fmt.Errorf("checkout.Unmanage: delete vfile: %w", err)
		}
	} else {
		// Existing tracked file — mark as deleted
		_, err := c.db.Exec("UPDATE vfile SET deleted=1 WHERE id=?", int64(vfileID))
		if err != nil {
			return fmt.Errorf("checkout.Unmanage: update vfile: %w", err)
		}
	}

	if callback != nil {
		if err := callback(pathname); err != nil {
			return fmt.Errorf("checkout.Unmanage: callback for %s: %w", pathname, err)
		}
	}

	return nil
}
