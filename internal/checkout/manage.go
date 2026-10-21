package checkout

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
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

	paths, err := c.expandManagePaths(opts.Paths)
	if err != nil {
		return counts, err
	}
	if err := c.refuseUncovered("Manage", paths...); err != nil {
		return counts, err
	}
	for _, path := range paths {
		tracked, err := c.isTracked(vid, path)
		if err != nil {
			return counts, err
		}
		if tracked {
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
		// Stat the on-disk file to capture its executable bit at add time.
		info, err := c.env.Storage.Stat(fullPath)
		if err != nil {
			return counts, fmt.Errorf("checkout.Manage: stat %s: %w", path, err)
		}
		isexe := 0
		if modeIsExecutable(info.Mode()) {
			isexe = 1
		}

		// Insert with rid=0 (newly added), chnged=1 (modified). islink is
		// written explicitly because a checkout made by fossil declares it
		// with no default. mhash stays NULL as fossil leaves it: it is only
		// meaningful during a merge.
		_, err = c.db.Exec(
			"INSERT INTO vfile(vid, pathname, rid, mrid, isexe, islink, chnged) "+
				"VALUES(?, ?, 0, 0, ?, 0, 1)",
			int64(vid), path, isexe,
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

// isTracked reports whether version vid's record has a row for path.
func (c *Checkout) isTracked(vid libfossil.FslID, path string) (bool, error) {
	var id int64
	err := c.db.QueryRow(
		"SELECT id FROM vfile WHERE vid=? AND pathname=?", int64(vid), path,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checkout.Manage: query vfile: %w", err)
	}
	return true, nil
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
			if err := c.refuseUncovered("Unmanage", pathname); err != nil {
				return err
			}

			if err := c.unmanageFile(vfileID, rid, pathname, opts.Callback); err != nil {
				return fmt.Errorf("checkout.Unmanage: %w", err)
			}
		}
		return nil
	}

	// Handle Paths
	if err := c.refuseUncovered("Unmanage", opts.Paths...); err != nil {
		return err
	}
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

// expandManagePaths replaces each named directory with the files beneath it,
// as fossil's add does: through the checkout's bounded, cone-aware walk,
// skipping any path with a component that starts with "." (fossil's default
// without --dotfiles). A named file is kept as given, dotfile or not.
func (c *Checkout) expandManagePaths(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		full, err := c.safePath(path)
		if err != nil {
			return nil, fmt.Errorf("checkout.Manage: path traversal in %s: %w", path, err)
		}
		info, statErr := c.env.Storage.Stat(full)
		if statErr != nil || !info.IsDir() {
			// An uncovered path is refused as such before its absence on
			// disk, which a sparse checkout makes ordinary.
			if err := c.refuseUncovered("Manage", path); err != nil {
				return nil, err
			}
			if statErr != nil {
				return nil, fmt.Errorf("checkout.Manage: stat %s: %w", path, statErr)
			}
			out = append(out, path)
			continue
		}
		files, err := c.walkDir(full)
		if err != nil {
			return nil, fmt.Errorf("checkout.Manage: %w", err)
		}
		names := make([]string, 0, len(files))
		for rel := range files {
			name := filepath.ToSlash(rel)
			if !hasDotComponent(name) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		out = append(out, names...)
	}
	return out, nil
}

func hasDotComponent(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}
