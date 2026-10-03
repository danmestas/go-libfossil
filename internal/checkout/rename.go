package checkout

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// moveFile reads a file from oldPath, writes it to newPath with the given
// permissions, and removes the original. Ignores NotExist on remove.
func (c *Checkout) moveFile(oldPath, newPath string, perm os.FileMode) error {
	data, err := c.env.Storage.ReadFile(oldPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", oldPath, err)
	}

	newParentDir := filepath.Dir(newPath)
	if err := c.env.Storage.MkdirAll(newParentDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", newParentDir, err)
	}

	if err := c.env.Storage.WriteFile(newPath, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", newPath, err)
	}

	if err := c.env.Storage.Remove(oldPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", oldPath, err)
	}

	return nil
}

// vfilePerm queries the isexe flag for a vfile row and returns the file mode.
func (c *Checkout) vfilePerm(vfileID int64) (os.FileMode, error) {
	var isexe int64
	err := c.db.QueryRow("SELECT CAST(isexe AS INTEGER) FROM vfile WHERE id=?", vfileID).Scan(&isexe)
	if err != nil {
		return 0, fmt.Errorf("query vfile permissions: %w", err)
	}
	if isexe != 0 {
		return 0o755, nil
	}
	return 0o644, nil
}

// modeIsExecutable is the inverse of vfilePerm: given an on-disk file mode,
// it reports whether the file should be recorded as executable.
//
// Matches Fossil's file_perm() predicate (src/file.c:316):
// S_ISREG(st_mode) && (S_IXUSR & st_mode) != 0 — owner-execute on a regular
// file. Directories, symlinks, and other non-regular files are never
// executable, and group- or other-only execute bits (e.g. 0645) do not
// qualify without the owner bit.
//
// On Windows, os.FileMode never carries an owner-execute bit, so this always
// returns false — matching Fossil's PERM_REG-only behavior on that platform.
func modeIsExecutable(mode os.FileMode) bool {
	return mode.IsRegular() && mode&0o100 != 0
}

// Rename marks a file as renamed in vfile by updating pathname and origname.
// Sets chnged=1 to indicate the file has been modified.
//
// If opts.DoFsMove is true, also moves the file in Storage from old to new path.
//
// Panics if c is nil, or if From or To is empty (TigerStyle precondition).
func (c *Checkout) Rename(opts RenameOpts) error {
	if c == nil {
		panic("checkout.Rename: nil *Checkout")
	}
	if opts.From == "" {
		panic("checkout.Rename: empty From")
	}
	if opts.To == "" {
		panic("checkout.Rename: empty To")
	}

	// Get current version
	vid, _, err := c.Version()
	if err != nil {
		return fmt.Errorf("checkout.Rename: %w", err)
	}

	vfileID, err := c.renameRowID(vid, opts.From, opts.To)
	if err != nil {
		return err
	}

	// Refuse to move onto a file already on disk, before changing anything:
	// it is not tracked (checked above), so it may be the only copy of
	// someone's work. fossil mv --hard refuses the same way.
	if opts.DoFsMove {
		if err := c.checkRenameTargetFree(opts.To); err != nil {
			return err
		}
	}

	// Update vfile: set new pathname, store old pathname in origname, mark as changed
	_, err = c.db.Exec(
		"UPDATE vfile SET pathname=?, origname=?, chnged=1 WHERE id=?",
		opts.To, opts.From, vfileID,
	)
	if err != nil {
		return fmt.Errorf("checkout.Rename: update vfile: %w", err)
	}

	// If DoFsMove, move the file in Storage
	if opts.DoFsMove {
		perm, err := c.vfilePerm(vfileID)
		if err != nil {
			return fmt.Errorf("checkout.Rename: %w", err)
		}

		oldPath, err := c.safePath(opts.From)
		if err != nil {
			return fmt.Errorf("checkout.Rename: path traversal in %s: %w", opts.From, err)
		}
		newPath, err := c.safePath(opts.To)
		if err != nil {
			return fmt.Errorf("checkout.Rename: path traversal in %s: %w", opts.To, err)
		}
		if err := c.moveFile(oldPath, newPath, perm); err != nil {
			return fmt.Errorf("checkout.Rename: %w", err)
		}
	}

	// Call callback if provided
	if opts.Callback != nil {
		if err := opts.Callback(opts.From, opts.To); err != nil {
			return fmt.Errorf("checkout.Rename: callback: %w", err)
		}
	}

	return nil
}

// RevertRename restores a renamed file to its original pathname.
// Returns (true, nil) if the revert succeeded, (false, nil) if there was nothing to revert.
//
// Panics if c is nil or name is empty (TigerStyle precondition).
func (c *Checkout) RevertRename(name string, doFsMove bool) (bool, error) {
	if c == nil {
		panic("checkout.RevertRename: nil *Checkout")
	}
	if name == "" {
		panic("checkout.RevertRename: empty name")
	}

	// Get current version
	vid, _, err := c.Version()
	if err != nil {
		return false, fmt.Errorf("checkout.RevertRename: %w", err)
	}

	// Look up vfile entry
	var vfileID int64
	var origname sql.NullString
	err = c.db.QueryRow(
		"SELECT id, origname FROM vfile WHERE vid=? AND pathname=?",
		int64(vid), name,
	).Scan(&vfileID, &origname)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("checkout.RevertRename: file %s not found in vfile", name)
	}
	if err != nil {
		return false, fmt.Errorf("checkout.RevertRename: query vfile for %s: %w", name, err)
	}

	// If origname is empty/NULL, nothing to revert
	if !origname.Valid || origname.String == "" {
		return false, nil
	}

	oldOrigName := origname.String

	// Update vfile: restore origname as pathname, clear origname, reset chnged
	_, err = c.db.Exec(
		"UPDATE vfile SET pathname=?, origname=NULL, chnged=0 WHERE id=?",
		oldOrigName, vfileID,
	)
	if err != nil {
		return false, fmt.Errorf("checkout.RevertRename: update vfile: %w", err)
	}

	// If doFsMove, move the file back in Storage
	if doFsMove {
		perm, err := c.vfilePerm(vfileID)
		if err != nil {
			return false, fmt.Errorf("checkout.RevertRename: %w", err)
		}

		currentPath, err := c.safePath(name)
		if err != nil {
			return false, fmt.Errorf("checkout.RevertRename: path traversal in %s: %w", name, err)
		}
		originalPath, err := c.safePath(oldOrigName)
		if err != nil {
			return false, fmt.Errorf("checkout.RevertRename: path traversal in %s: %w", oldOrigName, err)
		}
		if err := c.moveFile(currentPath, originalPath, perm); err != nil {
			return false, fmt.Errorf("checkout.RevertRename: %w", err)
		}
	}

	return true, nil
}

// checkRenameTargetFree returns an error if anything already exists on disk
// at the checkout-relative path to.
func (c *Checkout) checkRenameTargetFree(to string) error {
	if to == "" {
		panic("checkout.checkRenameTargetFree: empty path")
	}
	newPath, err := c.safePath(to)
	if err != nil {
		return fmt.Errorf("checkout.Rename: path traversal in %s: %w", to, err)
	}
	_, err = c.env.Storage.Stat(newPath)
	if err == nil {
		return fmt.Errorf(
			"checkout.Rename: cannot move onto %s: something already exists there", to)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checkout.Rename: stat %s: %w", newPath, err)
	}
	return nil
}

// renameRowID returns the vfile id of from in version vid, after checking
// that to is not tracked already.
func (c *Checkout) renameRowID(vid libfossil.FslID, from, to string) (int64, error) {
	if from == to {
		return 0, fmt.Errorf("checkout.Rename: %s is already named %s", from, to)
	}

	var vfileID int64
	err := c.db.QueryRow(
		"SELECT id FROM vfile WHERE vid=? AND pathname=?", int64(vid), from,
	).Scan(&vfileID)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("checkout.Rename: file %s not found in vfile", from)
	}
	if err != nil {
		return 0, fmt.Errorf("checkout.Rename: query vfile for %s: %w", from, err)
	}

	var existingID int64
	err = c.db.QueryRow(
		"SELECT id FROM vfile WHERE vid=? AND pathname=?", int64(vid), to,
	).Scan(&existingID)
	if err == nil {
		return 0, fmt.Errorf("checkout.Rename: target file %s already exists in vfile", to)
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("checkout.Rename: query vfile for %s: %w", to, err)
	}
	return vfileID, nil
}
