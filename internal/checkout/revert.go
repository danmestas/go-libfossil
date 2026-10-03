package checkout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// Revert restores files to their checkout version state, the way fossil's
// revert does. Edits to the reverted files are discarded: fossil saves an undo
// copy first, but go-libfossil has no undo yet, so they are gone. If opts.Paths is empty, reverts ALL changed files. Change
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
// it is renamed, or it is missing from disk.
func (c *Checkout) revertAll(vid libfossil.FslID, callback func(string, RevertChange) error) error {
	if vid < 0 {
		panic("checkout.revertAll: negative vid")
	}

	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil {
		return err
	}
	inVersion, err := c.versionPaths(vid)
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
		if err := c.revertFile(r, inVersion, callback); err != nil {
			return err
		}
	}
	if _, err := c.db.Exec("DELETE FROM vmerge"); err != nil {
		return fmt.Errorf("clear vmerge: %w", err)
	}
	return nil
}

// needsRevert reports whether revert-all restores row r: its content
// changed, it is removed or renamed, a merge touched it, or it is missing
// from disk.
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
	if r.RenamedFrom() != "" {
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
	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil {
		return err
	}
	for _, r := range rows {
		// A renamed file answers to its new name and its old one, as in
		// fossil's revert.
		if r.Pathname != pathname && r.RenamedFrom() != pathname {
			continue
		}
		inVersion, err := c.versionPaths(vid)
		if err != nil {
			return err
		}
		return c.revertFile(r, inVersion, callback)
	}
	return nil
}

// versionPaths returns the set of pathnames in checked-out version vid's
// manifest; empty when nothing is checked out yet.
func (c *Checkout) versionPaths(vid libfossil.FslID) (map[string]bool, error) {
	paths := make(map[string]bool)
	if vid <= 0 {
		return paths, nil
	}
	files, err := manifest.ListFiles(c.repo, vid)
	if err != nil {
		return nil, fmt.Errorf("list version files: %w", err)
	}
	for _, f := range files {
		paths[f.Name] = true
	}
	return paths, nil
}

// revertFile reverts one row the way fossil's revert does, by whether the
// checked-out version has the file (under its name, or its name before a
// pending rename):
//   - a pending add (rid=0) is un-managed and left on disk: there is no
//     committed copy, so deleting it would destroy the only one;
//   - a file the version lacks but the checkout tracks (one a merge added)
//     is deleted with its row: its content lives on in the merged-in version;
//   - a pending rename is undone: the file under the new name is removed and
//     the committed content is restored under the old one;
//   - anything else gets its committed content back.
func (c *Checkout) revertFile(
	r vfile.Row, inVersion map[string]bool,
	callback func(string, RevertChange) error,
) error {
	if inVersion == nil {
		panic("checkout.revertFile: nil inVersion")
	}
	fullPath, err := c.safePath(r.Pathname)
	if err != nil {
		return fmt.Errorf("checkout.Revert: path traversal in %s: %w", r.Pathname, err)
	}

	change := RevertContents
	switch {
	case r.IsAdded():
		change = RevertUnmanage
		if _, err := c.db.Exec("DELETE FROM vfile WHERE id = ?", r.ID); err != nil {
			return fmt.Errorf("checkout.Revert: delete vfile for %s: %w", r.Pathname, err)
		}
	case !inVersion[r.Pathname] && !inVersion[r.RenamedFrom()]:
		change = RevertRemove
		if err := c.removeMergeAdded(r, fullPath); err != nil {
			return err
		}
	case r.RenamedFrom() != "":
		change = RevertRename
		if err := c.undoRename(r, fullPath); err != nil {
			return err
		}
	default:
		if err := c.restoreCommitted(r.ID, r.Pathname, fullPath, r.RID); err != nil {
			return err
		}
	}
	if callback != nil {
		if err := callback(r.Pathname, change); err != nil {
			return fmt.Errorf("checkout.Revert: callback for %s: %w", r.Pathname, err)
		}
	}
	return nil
}

// undoRename moves a pending rename back, as fossil's revert does: the file
// at the new name (fullPath) is removed, the row takes its old name again,
// and the committed content is written there. UPDATE OR REPLACE matches
// fossil: a row already holding the old name gives way.
func (c *Checkout) undoRename(r vfile.Row, fullPath string) error {
	oldName := r.RenamedFrom()
	if oldName == "" {
		panic("checkout.undoRename: row records no rename")
	}
	if r.RID <= 0 {
		panic("checkout.undoRename: a pending add has no committed name")
	}

	oldPath, err := c.safePath(oldName)
	if err != nil {
		return fmt.Errorf("checkout.Revert: path traversal in %s: %w", oldName, err)
	}
	if err := c.removeRegularFile(fullPath); err != nil {
		return err
	}
	if _, err := c.db.Exec(
		"UPDATE OR REPLACE vfile SET pathname = ?, origname = NULL WHERE id = ?",
		oldName, r.ID,
	); err != nil {
		return fmt.Errorf("checkout.Revert: rename %s back to %s: %w", r.Pathname, oldName, err)
	}
	return c.restoreCommitted(r.ID, oldName, oldPath, r.RID)
}

// removeRegularFile deletes fullPath if a regular file is there; anything
// else (nothing, or a directory the user made) is left alone.
func (c *Checkout) removeRegularFile(fullPath string) error {
	if fullPath == "" {
		panic("checkout.removeRegularFile: empty path")
	}
	info, err := c.env.Storage.Stat(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checkout.Revert: stat %s: %w", fullPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if err := c.env.Storage.Remove(fullPath); err != nil {
		return fmt.Errorf("checkout.Revert: remove %s: %w", fullPath, err)
	}
	return nil
}

// removeMergeAdded deletes a file the checked-out version does not have,
// from disk and from vfile.
func (c *Checkout) removeMergeAdded(r vfile.Row, fullPath string) error {
	if r.RID <= 0 {
		panic("checkout.removeMergeAdded: pending add has no merged-in content")
	}
	// Only a regular file is the merge's: anything else at that path (a
	// directory the user made) is left alone.
	if err := c.removeRegularFile(fullPath); err != nil {
		return err
	}
	if _, err := c.db.Exec("DELETE FROM vfile WHERE id = ?", r.ID); err != nil {
		return fmt.Errorf("checkout.Revert: delete vfile for %s: %w", r.Pathname, err)
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
