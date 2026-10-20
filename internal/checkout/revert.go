package checkout

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// Revert restores files to their checkout version state, the way fossil's
// revert does. Each file it writes or deletes is saved for undo first
// (except under js/wasm, which has no undo), so Undo brings back the edits
// it discarded. If opts.Paths is empty, reverts ALL changed files. Change
// flags are refreshed from disk first, so an edit not yet scanned is
// reverted too.
//
// For each file to revert:
//   - If rid==0 (newly added): un-manage it (DELETE from vfile). The file stays
//     on disk: it was never committed, so deleting it would destroy the only copy.
//   - If rid>0 (existing, modified/deleted): restore original content, reset chnged=0, deleted=0
//   - If renamed: move it back to its committed name, whatever holds that
//     name now (see planRevertSteps).
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
		if err := c.revertPaths(vid, opts.Paths, opts.Callback); err != nil {
			return fmt.Errorf("checkout.Revert: %w", err)
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
	var targets []vfile.Row
	for _, r := range rows {
		missing, err := c.isMissing(r)
		if err != nil {
			return err
		}
		if needsRevert(r, missing) {
			targets = append(targets, r)
		}
	}
	return c.revertRows(rows, targets, inVersion, true, callback)
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

// revertPaths reverts the named files whatever their change flags say, as
// fossil's revert does for files it is given: a file missing from disk is
// restored too. A renamed file answers to its new name and its old one, so a
// name can select more than one row. A path the checkout does not track is
// a no-op, as in fossil.
func (c *Checkout) revertPaths(
	vid libfossil.FslID, paths []string,
	callback func(string, RevertChange) error,
) error {
	if len(paths) == 0 {
		panic("checkout.revertPaths: no paths")
	}
	named := make(map[string]bool, len(paths))
	for _, p := range paths {
		named[p] = true
	}
	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil {
		return err
	}
	var targets []vfile.Row
	for _, r := range rows {
		if named[r.Pathname] || named[r.RenamedFrom()] {
			targets = append(targets, r)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	inVersion, err := c.versionPaths(vid)
	if err != nil {
		return err
	}
	return c.revertRows(rows, targets, inVersion, false, callback)
}

// revertAction is what revert does to one row. planRevertSteps decides it
// for every row before anything changes.
type revertAction int

const (
	revertRestore revertAction = iota
	revertUnmanage
	revertRemoveMergeAdded
	revertUndoRename
	// revertDisplace drops a row that holds the name a rename goes back to,
	// as fossil's UPDATE OR REPLACE does; the renamed file is written over
	// its file.
	revertDisplace
)

// planRevert decides a row's revert the way fossil's revert does, by whether
// the checked-out version has the file (under its name, or its name before a
// pending rename):
//   - a pending add (rid=0) is un-managed and left on disk: there is no
//     committed copy, so deleting it would destroy the only one;
//   - a file the version lacks but the checkout tracks (one a merge added)
//     is deleted with its row: its content lives on in the merged-in version;
//   - a pending rename is undone: the file under the new name is removed and
//     the committed content is restored under the old one;
//   - anything else gets its committed content back.
func planRevert(r vfile.Row, inVersion map[string]bool) revertAction {
	if inVersion == nil {
		panic("checkout.planRevert: nil inVersion")
	}
	if r.IsAdded() {
		return revertUnmanage
	}
	if !inVersion[r.Pathname] && !inVersion[r.RenamedFrom()] {
		return revertRemoveMergeAdded
	}
	if r.RenamedFrom() != "" {
		return revertUndoRename
	}
	return revertRestore
}

// revertStep is one row's part in a revert.
type revertStep struct {
	row        vfile.Row
	action     revertAction
	recordOnly bool
}

// planRevertSteps plans the revert of targets, chosen from rows (every row
// of the version). Undoing a rename takes back the old name from whatever
// row holds it now, as fossil's revert does: a row renamed onto it (a swap
// or a chain) is reverted too, to its own old name, and any other row there
// (a new add) is dropped, its file overwritten. Fossil leaves a swap's rows
// renamed and drops a chain's displaced row; here every committed file ends
// under its committed name.
func planRevertSteps(rows, targets []vfile.Row, inVersion map[string]bool) []revertStep {
	byPath := make(map[string]vfile.Row, len(rows))
	for _, r := range rows {
		byPath[r.Pathname] = r
	}
	steps := make([]revertStep, 0, len(targets))
	planned := make(map[int64]int, len(targets)) // row id -> index in steps
	add := func(r vfile.Row) {
		planned[r.ID] = len(steps)
		steps = append(steps, revertStep{row: r, action: planRevert(r, inVersion)})
	}
	for _, t := range targets {
		if _, ok := planned[t.ID]; !ok {
			add(t)
		}
	}
	// steps grows as renames pull in the rows holding their old names; each
	// row is planned once, so the loop ends.
	for i := 0; i < len(steps); i++ {
		if steps[i].action != revertUndoRename {
			continue
		}
		holder, ok := byPath[steps[i].row.RenamedFrom()]
		if !ok || holder.ID == steps[i].row.ID {
			continue
		}
		j, ok := planned[holder.ID]
		if !ok {
			add(holder)
			j = len(steps) - 1
		}
		if steps[j].action == revertRestore {
			steps[j].action = revertDisplace
		}
	}
	if len(steps) > len(rows) {
		panic("checkout.planRevertSteps: more steps than rows")
	}
	return steps
}

// revertRows reverts targets, chosen from rows (every row of the version),
// and with clearMerge drops the pending merge. Like fossil's revert it saves
// every file it writes or deletes for undo, then changes vfile and the files
// in one transaction; should a step fail, the transaction rolls back and the
// saved files are put back, as fossil's undo_rollback does.
func (c *Checkout) revertRows(
	rows, targets []vfile.Row, inVersion map[string]bool, clearMerge bool,
	callback func(string, RevertChange) error,
) (err error) {
	steps := planRevertSteps(rows, targets, inVersion)
	for i := range steps {
		steps[i].recordOnly = !c.covers(steps[i].row.Pathname)
	}
	saved, err := c.readRevertUndo(steps)
	if err != nil {
		return err
	}
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("begin revert: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, tx.Rollback(), c.rollBackRevert(saved))
		}
	}()
	if err := saved.save(tx); err != nil {
		return fmt.Errorf("save undo state: %w", err)
	}
	if clearMerge {
		if _, err := tx.Exec("DELETE FROM vmerge"); err != nil {
			return fmt.Errorf("clear vmerge: %w", err)
		}
	}
	if err := c.applyRevert(tx, steps, callback); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit revert: %w", err)
	}
	committed = true
	return nil
}

// revertNames returns the names whose files steps write or delete, each
// once, for undo: a row's own name unless it is only un-managed or dropped,
// and a rename's old name.
func revertNames(steps []revertStep) []string {
	seen := make(map[string]bool, len(steps))
	var names []string
	note := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, s := range steps {
		if s.recordOnly {
			continue
		}
		switch s.action {
		case revertUnmanage, revertDisplace:
		case revertUndoRename:
			note(s.row.Pathname)
			note(s.row.RenamedFrom())
		default:
			note(s.row.Pathname)
		}
	}
	return names
}

// applyRevert carries out steps within tx, in three passes so a swap or a
// chain never writes a file or a row over one still to move: first every
// file leaving its name is removed and every row leaving vfile deleted, a
// renamed row parked on a placeholder name; then the renamed rows take
// their old names; then committed content is written. Each step is
// reported to callback, under the name its file ends up with.
func (c *Checkout) applyRevert(
	tx *sql.Tx, steps []revertStep, callback func(string, RevertChange) error,
) error {
	paths := make([]string, len(steps))
	for i, s := range steps {
		if s.recordOnly {
			continue
		}
		name := s.row.Pathname
		if s.action == revertUndoRename {
			name = s.row.RenamedFrom()
		}
		p, err := c.safePath(name)
		if err != nil {
			return fmt.Errorf("path traversal in %s: %w", name, err)
		}
		paths[i] = p
	}
	for _, s := range steps {
		if err := c.vacateRevertRow(tx, s); err != nil {
			return err
		}
	}
	for _, s := range steps {
		if s.action != revertUndoRename {
			continue
		}
		if _, err := tx.Exec(
			"UPDATE vfile SET pathname = ?, origname = NULL WHERE id = ?",
			s.row.RenamedFrom(), s.row.ID,
		); err != nil {
			return fmt.Errorf("rename %s back to %s: %w", s.row.Pathname, s.row.RenamedFrom(), err)
		}
	}
	for i, s := range steps {
		name, change := s.row.Pathname, RevertContents
		switch s.action {
		case revertRestore, revertUndoRename:
			if s.action == revertUndoRename {
				name, change = s.row.RenamedFrom(), RevertRename
			}
			if err := c.restoreCommitted(tx, s.row.ID, name, paths[i], s.row.RID); err != nil {
				return err
			}
		case revertUnmanage:
			change = RevertUnmanage
		case revertRemoveMergeAdded:
			change = RevertRemove
		case revertDisplace:
			continue
		default:
			panic("checkout.applyRevert: unknown action")
		}
		if callback != nil {
			if err := callback(name, change); err != nil {
				return fmt.Errorf("callback for %s: %w", name, err)
			}
		}
	}
	return nil
}

// vacateRevertRow is applyRevert's first pass for step s: a renamed file is
// removed from its new name and its row parked on a placeholder, a file a
// merge added is removed with its row, and an add or a displaced row leaves
// vfile, its file left on disk.
func (c *Checkout) vacateRevertRow(tx *sql.Tx, s revertStep) error {
	switch s.action {
	case revertRestore:
		return nil
	case revertUndoRename:
		if s.row.RID <= 0 {
			panic("checkout.vacateRevertRow: a pending add has no committed name")
		}
		if !s.recordOnly {
			if err := c.removeTrackedFile(s.row.Pathname); err != nil {
				return err
			}
		}
		// A leading slash is never a checkout path, so the placeholder
		// cannot collide with a row.
		parked := "/revert/" + strconv.FormatInt(s.row.ID, 10)
		if _, err := tx.Exec("UPDATE vfile SET pathname = ? WHERE id = ?", parked, s.row.ID); err != nil {
			return fmt.Errorf("move %s aside: %w", s.row.Pathname, err)
		}
		return nil
	case revertRemoveMergeAdded:
		if s.row.RID <= 0 {
			panic("checkout.vacateRevertRow: pending add has no merged-in content")
		}
		if !s.recordOnly {
			if err := c.removeTrackedFile(s.row.Pathname); err != nil {
				return err
			}
		}
	case revertUnmanage, revertDisplace:
	default:
		panic("checkout.vacateRevertRow: unknown action")
	}
	if _, err := tx.Exec("DELETE FROM vfile WHERE id = ?", s.row.ID); err != nil {
		return fmt.Errorf("delete vfile for %s: %w", s.row.Pathname, err)
	}
	return nil
}

// removeTrackedFile removes the regular file a row names, if there is one.
func (c *Checkout) removeTrackedFile(name string) error {
	fullPath, err := c.safePath(name)
	if err != nil {
		return fmt.Errorf("path traversal in %s: %w", name, err)
	}
	return c.removeRegularFile(fullPath)
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

// removeRegularFile deletes fullPath if a file is there, a symlink seen as
// itself (vfile.Disk, as fossil's file_delete removes the link); anything
// else (nothing, or a directory the user made) is left alone.
func (c *Checkout) removeRegularFile(fullPath string) error {
	if fullPath == "" {
		panic("checkout.removeRegularFile: empty path")
	}
	disk, err := c.newDiskView()
	if err != nil {
		return fmt.Errorf("checkout: %w", err)
	}
	info, err := disk.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("checkout: stat %s: %w", fullPath, err)
	}
	if !isFileOrLink(info) {
		return nil
	}
	if err := c.env.Storage.Remove(fullPath); err != nil {
		return fmt.Errorf("checkout: remove %s: %w", fullPath, err)
	}
	return nil
}

// restoreCommitted writes the committed content of blob rid back to fullPath,
// with the row's executable and symlink bits, as fossil's revert does
// (vfile.Disk.Write: a symlink is made a symlink again, and nothing is
// written through one), and marks row id unchanged and unmerged, within tx.
func (c *Checkout) restoreCommitted(tx *sql.Tx, id int64, pathname, fullPath string, rid int64) error {
	if rid <= 0 {
		panic("checkout.restoreCommitted: rid must be positive")
	}
	if fullPath == "" {
		var vid int64
		if err := tx.QueryRow("SELECT vid FROM vfile WHERE id=?", id).Scan(&vid); err != nil {
			return err
		}
		file, present, err := manifest.LookupFile(c.repo, libfossil.FslID(vid), pathname)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("checkout.Revert: no baseline entry for %s", pathname)
		}
		_, err = tx.Exec(`UPDATE vfile SET chnged=0, deleted=0, mrid=rid,
			mhash=NULL, mtime=0, isexe=?, islink=? WHERE id=?`, file.Perm == "x", file.Perm == "l", id)
		return err
	}

	data, err := content.Expand(c.repo.DB(), libfossil.FslID(rid))
	if err != nil {
		return fmt.Errorf("checkout.Revert: expand blob for %s: %w", pathname, err)
	}
	var isexe, islink int64
	err = tx.QueryRow(
		"SELECT CAST(isexe AS INTEGER), CAST(coalesce(islink, 0) AS INTEGER) FROM vfile WHERE id = ?", id,
	).Scan(&isexe, &islink)
	if err != nil {
		return fmt.Errorf("checkout.Revert: query vfile metadata for %s: %w", pathname, err)
	}
	disk, err := c.newDiskView()
	if err != nil {
		return fmt.Errorf("checkout.Revert: %w", err)
	}
	if err := disk.Write(fullPath, data, isexe != 0, islink != 0); err != nil {
		return fmt.Errorf("checkout.Revert: %w", err)
	}
	mtime, err := disk.MTime(fullPath)
	if err != nil {
		return fmt.Errorf("checkout.Revert: %w", err)
	}
	// mrid and mhash go back to the committed version, so no merged-in
	// content outlives the revert, and the mtime is the restored file's
	// (fossil resets them the same way).
	if _, err := tx.Exec(
		"UPDATE vfile SET chnged = 0, deleted = 0, mrid = rid, mhash = NULL, mtime = ? WHERE id = ?",
		mtime, id,
	); err != nil {
		return fmt.Errorf("checkout.Revert: update vfile for %s: %w", pathname, err)
	}
	return nil
}
