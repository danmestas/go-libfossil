package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// Update moves the checkout to another version the way fossil's update
// does (src/update.c), carrying uncommitted changes into it. It first scans
// the checkout, then for each file:
//
//   - one without local changes takes the target's content, or is removed
//     when the target does not have it;
//   - one edited locally that the target also changed is merged three ways:
//     the checkout's version is the base, the working file the local side
//     and the target the other; a merge that cannot resolve writes conflict
//     markers and is listed in UpdateResult.Conflicted;
//   - one edited locally that the target deletes is kept on disk, no longer
//     tracked;
//   - a pending add stays added, and a pending removal stays removed;
//   - a file the target adds over an untracked file is written after the
//     untracked one is copied to NAME-original;
//   - a binary file edited locally and changed by the target is not merged:
//     the working copy is saved as NAME-original and the target written.
//
// Every file then gets the target's executable bit. An update that could
// not finish (a directory, or a read-only file, where a file must be
// written) is refused before anything is written, and the checkout
// database moves to the target in one transaction. A dry run reports the
// same result and changes nothing, on disk or in the checkout database.
//
// Unlike fossil, nothing follows a file the target renamed: files are
// matched by name, and no undo state is saved.
//
// If opts.TargetRID is 0, CalcUpdateVersion is used to find the target.
// If the target equals the current version, Update returns a zero-value
// UpdateResult and a nil error (nothing to do).
//
// A non-nil error means the update genuinely failed; the returned
// UpdateResult's contents are not meaningful in that case (see UpdateResult
// for the outcome mapping). Update never returns a non-nil error together
// with a non-empty UpdateResult.Conflicted — a conflicted merge is a
// success, not a failure.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Update(opts UpdateOpts) (UpdateResult, error) {
	if c == nil {
		panic("checkout.Update: nil *Checkout")
	}

	target, current, err := c.updateTarget(opts.TargetRID)
	if err != nil {
		return UpdateResult{}, err
	}
	if target == 0 {
		return UpdateResult{}, nil // nothing to update, or already there
	}

	ctx := c.obs.ExtractStarted(context.Background(), ExtractStart{
		Operation: "update",
		TargetRID: target,
	})
	var res UpdateResult
	var updateErr error
	defer func() {
		c.obs.ExtractCompleted(ctx, ExtractEnd{
			Operation:    "update",
			TargetRID:    target,
			FilesWritten: len(res.FilesWritten),
			FilesRemoved: len(res.FilesRemoved),
			Conflicts:    len(res.Conflicted),
			Err:          updateErr,
		})
	}()

	steps, err := c.planUpdate(current, target, opts.DryRun)
	if err == nil && !opts.DryRun {
		err = c.checkUpdateTargets(steps)
	}
	if err != nil {
		updateErr = fmt.Errorf("checkout.Update: %w", err)
		return UpdateResult{}, updateErr
	}
	fresh := map[string]writtenFile{}
	if res, updateErr = c.applyUpdate(ctx, steps, opts, fresh); updateErr != nil {
		return UpdateResult{}, updateErr
	}
	if opts.DryRun {
		return res, nil
	}
	if updateErr = c.finalizeUpdate(target, steps, fresh); updateErr != nil {
		return UpdateResult{}, updateErr
	}
	if opts.SetMTime {
		if updateErr = c.setFileMTimes(target); updateErr != nil {
			return UpdateResult{}, updateErr
		}
	}
	return res, nil
}

// updateAction is what fossil's update does with one file.
type updateAction int

const (
	updateNothing       updateAction = iota // same artifact in both, or edited and unchanged by the target
	updateWrite                             // take the target's content: new, unedited, or missing locally
	updateAddOver                           // the target adds it over an untracked file: back that up, then write
	updateMerge                             // edited locally and changed by the target: merge
	updateRemove                            // unedited, and the target does not have it: delete it
	updateKeepEdited                        // edited locally, and the target does not have it: keep it, untracked
	updateKeepDuplicate                     // added locally and by the target: keep the local file
	updateKeepAdd                           // added locally only: it stays a pending add
)

// updateTargetFile is a file of the version Update moves to.
type updateTargetFile struct {
	rid int64
	exe bool
}

// writtenFile is a file Update wrote with the target's artifact rid, and
// the vfile.mtime that write gave it.
type writtenFile struct {
	rid, mtime int64
}

// updateStep is one file's part in an update.
type updateStep struct {
	name   string
	row    *vfile.Row        // the current version's row, nil when it has none
	target *updateTargetFile // nil when the target does not have the file
	action updateAction
}

// classifyUpdate is fossil's update loop deciding one file, in its order.
// missing reports that row's file is not on disk; unmanaged that an
// untracked file stands where the target adds one. As fossil's scan leaves
// them, a removed or missing file counts as changed.
func classifyUpdate(row *vfile.Row, tgt *updateTargetFile, missing, unmanaged bool) updateAction {
	if row == nil && tgt == nil {
		panic("checkout.classifyUpdate: file in neither version")
	}
	if row == nil {
		if unmanaged {
			return updateAddOver
		}
		return updateWrite
	}
	deleted := row.Deleted > 0
	changed := row.Chnged > 0 || deleted || missing
	switch {
	case row.RID == 0 && tgt != nil:
		return updateKeepDuplicate
	case tgt != nil && tgt.rid != row.RID && (!changed || deleted):
		return updateWrite
	case tgt != nil && !deleted && missing:
		return updateWrite
	case tgt == nil && row.RID == 0:
		return updateKeepAdd
	case tgt == nil && changed:
		return updateKeepEdited
	case tgt == nil:
		return updateRemove
	case tgt.rid != row.RID:
		return updateMerge
	}
	return updateNothing
}

// updateTarget picks the version Update moves to and the current one. A
// target of 0 means there is nothing to do: no newer version, or the
// checkout is already there.
func (c *Checkout) updateTarget(
	requested libfossil.FslID,
) (target, current libfossil.FslID, err error) {
	if c == nil {
		panic("checkout.updateTarget: nil *Checkout")
	}
	if requested < 0 {
		return 0, 0, fmt.Errorf("checkout.Update: invalid target rid %d", requested)
	}

	target = requested
	if target == 0 {
		target, err = c.CalcUpdateVersion()
		if err != nil {
			return 0, 0, fmt.Errorf("checkout.Update: %w", err)
		}
		if target == 0 {
			return 0, 0, nil
		}
	}
	current, _, err = c.Version()
	if err != nil {
		return 0, 0, fmt.Errorf("checkout.Update: %w", err)
	}
	if current == target {
		return 0, current, nil
	}
	if target <= 0 {
		panic("checkout.updateTarget: chose a non-positive target")
	}
	return target, current, nil
}

// planUpdate scans the checkout, as fossil's update does first, and decides
// each file of the current version vid and the target. It refuses a target
// whose content is not all here before anything is written. A dry run keeps
// the scan's findings to itself.
func (c *Checkout) planUpdate(vid, target libfossil.FslID, dryRun bool) ([]updateStep, error) {
	files, rids, err := c.resolveFiles(target)
	if err != nil {
		return nil, err
	}
	var scanned []scanWrite
	if vid > 0 {
		if scanned, err = c.scanVersion(vid); err != nil {
			return nil, err
		}
	}
	if !dryRun {
		if err := c.storeScan(scanned); err != nil {
			return nil, err
		}
	}
	targets := make(map[string]*updateTargetFile, len(files))
	for i, f := range files {
		targets[f.Name] = &updateTargetFile{rid: int64(rids[i]), exe: strings.Contains(f.Perm, "x")}
	}
	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil {
		return nil, err
	}
	overlayScan(rows, scanned)
	steps := make([]updateStep, 0, len(rows)+len(files))
	for i := range rows {
		row := &rows[i]
		missing, err := c.isMissing(*row)
		if err != nil {
			return nil, err
		}
		tgt := targets[row.Pathname]
		delete(targets, row.Pathname)
		steps = append(steps, updateStep{
			name: row.Pathname, row: row, target: tgt,
			action: classifyUpdate(row, tgt, missing, false),
		})
	}
	for _, f := range files {
		tgt, ok := targets[f.Name]
		if !ok {
			continue
		}
		unmanaged, err := c.isUntrackedFile(f.Name)
		if err != nil {
			return nil, err
		}
		steps = append(steps, updateStep{
			name: f.Name, target: tgt, action: classifyUpdate(nil, tgt, false, unmanaged),
		})
	}
	return steps, nil
}

// isUntrackedFile reports whether a regular file stands at name.
func (c *Checkout) isUntrackedFile(name string) (bool, error) {
	fullPath, err := c.safePath(name)
	if err != nil {
		return false, err
	}
	info, err := c.env.Storage.Stat(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", fullPath, err)
	}
	return info.Mode().IsRegular(), nil
}

// applyUpdate carries out the steps, reports each to the observer and the
// caller's callback, and records in fresh the mtime of each file written
// with the target's content. With opts.DryRun nothing is written.
func (c *Checkout) applyUpdate(
	ctx context.Context, steps []updateStep, opts UpdateOpts, fresh map[string]writtenFile,
) (UpdateResult, error) {
	strategy, ok := merge.StrategyByName("three-way")
	if !ok {
		return UpdateResult{}, fmt.Errorf("checkout.Update: three-way merge strategy not registered")
	}
	var res UpdateResult
	for _, s := range steps {
		change, err := c.applyUpdateStep(s, strategy, opts.DryRun, fresh)
		if err != nil {
			return UpdateResult{}, fmt.Errorf("checkout.Update: %s: %w", s.name, err)
		}
		switch change {
		case UpdateNone:
			continue
		case UpdateAdded, UpdateUpdated, UpdateMerged, UpdateConflictUnmanaged:
			res.FilesWritten = append(res.FilesWritten, s.name)
		case UpdateConflictMerged:
			res.FilesWritten = append(res.FilesWritten, s.name)
			res.Conflicted = append(res.Conflicted, s.name)
			c.obs.Error(ctx, fmt.Errorf("checkout.Update: merge conflict in %s", s.name))
		case UpdateConflictBinary:
			res.FilesWritten = append(res.FilesWritten, s.name)
			res.Unmerged = append(res.Unmerged, s.name)
		case UpdateConflictRm, UpdateConflictAdded:
			res.Unmerged = append(res.Unmerged, s.name)
		case UpdateRemoved:
			res.FilesRemoved = append(res.FilesRemoved, s.name)
		}
		c.obs.ExtractFileCompleted(ctx, s.name, change)
		if opts.Callback != nil {
			if err := opts.Callback(s.name, change); err != nil {
				return UpdateResult{}, fmt.Errorf("checkout.Update: callback for %s: %w", s.name, err)
			}
		}
	}
	if !opts.DryRun {
		if err := c.setUpdatedModes(steps); err != nil {
			return UpdateResult{}, fmt.Errorf("checkout.Update: %w", err)
		}
	}
	sort.Strings(res.FilesWritten)
	sort.Strings(res.FilesRemoved)
	sort.Strings(res.Conflicted)
	sort.Strings(res.Unmerged)
	return res, nil
}

// applyUpdateStep carries out one step and names the change it made.
func (c *Checkout) applyUpdateStep(
	s updateStep, strategy merge.Strategy, dryRun bool, fresh map[string]writtenFile,
) (UpdateChange, error) {
	switch s.action {
	case updateNothing:
		return UpdateNone, nil
	case updateWrite, updateAddOver:
		change := UpdateUpdated
		if s.row == nil {
			change = UpdateAdded
		}
		if s.action == updateAddOver {
			change = UpdateConflictUnmanaged
		}
		if dryRun {
			return change, nil
		}
		if s.action == updateAddOver {
			if err := c.backUpOriginal(s.name); err != nil {
				return UpdateNone, err
			}
		}
		return change, c.writeUpdateTarget(s, fresh)
	case updateMerge:
		return c.mergeUpdate(s, strategy, dryRun, fresh)
	case updateRemove:
		if dryRun {
			return UpdateRemoved, nil
		}
		return UpdateRemoved, c.removeUpdated(s.name)
	case updateKeepEdited:
		return UpdateConflictRm, nil
	case updateKeepDuplicate:
		return UpdateConflictAdded, nil
	case updateKeepAdd:
		return UpdateAddPropagated, nil
	}
	panic(fmt.Sprintf("checkout.applyUpdateStep: unknown action %d", s.action))
}

// writeUpdateTarget writes the target's content for s and records in fresh
// the mtime the write gave it.
func (c *Checkout) writeUpdateTarget(s updateStep, fresh map[string]writtenFile) error {
	data, err := content.Expand(c.repo.DB(), libfossil.FslID(s.target.rid))
	if err != nil {
		return fmt.Errorf("expand rid %d: %w", s.target.rid, err)
	}
	if err := c.writeMergedFile(s.name, data, s.target.exe); err != nil {
		return err
	}
	fullPath, err := c.safePath(s.name)
	if err != nil {
		return err
	}
	mtime, err := c.writtenMTime(fullPath)
	if err != nil {
		return err
	}
	fresh[s.name] = writtenFile{rid: s.target.rid, mtime: mtime}
	return nil
}

// mergeUpdate merges the working file's changes into the target's content:
// the checkout's version of the file is the base, the working file the
// local side, the target the other, as fossil's update calls merge_3way.
//
// Binary content is not line-merged: as fossil does, the working copy is
// saved as NAME-original and the target's content written.
func (c *Checkout) mergeUpdate(
	s updateStep, strategy merge.Strategy, dryRun bool, fresh map[string]writtenFile,
) (UpdateChange, error) {
	if s.row == nil || s.row.RID <= 0 || s.target == nil {
		panic("checkout.mergeUpdate: merge needs both versions")
	}
	base, err := content.Expand(c.repo.DB(), libfossil.FslID(s.row.RID))
	if err != nil {
		return UpdateNone, fmt.Errorf("expand rid %d: %w", s.row.RID, err)
	}
	local, present, err := c.readWorkingFile(*s.row)
	if err != nil {
		return UpdateNone, err
	}
	if !present {
		panic("checkout.mergeUpdate: a missing file is written, not merged")
	}
	other, err := content.Expand(c.repo.DB(), libfossil.FslID(s.target.rid))
	if err != nil {
		return UpdateNone, fmt.Errorf("expand rid %d: %w", s.target.rid, err)
	}
	if !(merge.IsText(base) && merge.IsText(local) && merge.IsText(other)) {
		if dryRun {
			return UpdateConflictBinary, nil
		}
		if err := c.backUpOriginal(s.name); err != nil {
			return UpdateNone, err
		}
		return UpdateConflictBinary, c.writeUpdateTarget(s, fresh)
	}
	merged, err := strategy.Merge(base, local, other)
	if err != nil {
		return UpdateNone, err
	}
	change := UpdateMerged
	if !merged.Clean {
		change = UpdateConflictMerged
	}
	if dryRun {
		return change, nil
	}
	// The row the target's version gets records no mtime, so the next scan
	// hashes the merged file and finds it edited.
	return change, c.writeMergedFile(s.name, merged.Content, s.target.exe)
}

// removeUpdated deletes name's file, which the target does not have. One
// already gone is fine.
func (c *Checkout) removeUpdated(name string) error {
	fullPath, err := c.safePath(name)
	if err != nil {
		return err
	}
	if err := c.env.Storage.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", fullPath, err)
	}
	return nil
}

// backUpOriginal copies the untracked file at name to NAME-original, or
// NAME-original-N for the first N free, as fossil's file_newname names it,
// before the target's file is written over it.
func (c *Checkout) backUpOriginal(name string) error {
	fullPath, err := c.safePath(name)
	if err != nil {
		return err
	}
	data, err := c.env.Storage.ReadFile(fullPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", fullPath, err)
	}
	backup := fullPath + "-original"
	for n := 0; ; n++ {
		if n > backupNamesMax {
			return fmt.Errorf("no free backup name for %s", fullPath)
		}
		if _, err := c.env.Storage.Stat(backup); errors.Is(err, os.ErrNotExist) {
			break
		}
		backup = fmt.Sprintf("%s-original-%d", fullPath, n)
	}
	if err := c.env.Storage.WriteFile(backup, data, 0o644); err != nil {
		return fmt.Errorf("back up %s: %w", fullPath, err)
	}
	return nil
}

// backupNamesMax bounds the search for a free backup name.
const backupNamesMax = 1000

// finalizeUpdate moves the checkout database to target in one
// transaction. A pending add keeps its row, now in the target version, as
// fossil's update moves it; the target's files are loaded, a file removed
// locally stays removed, and each file written with the target's content
// records the mtime of that write (an unchanged file keeps its mtime
// through loadVFile, as fossil's update transfers it). Last, vvar names the
// new version.
func (c *Checkout) finalizeUpdate(
	target libfossil.FslID, steps []updateStep, fresh map[string]writtenFile,
) (err error) {
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("checkout.Update: begin: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	if err := c.moveUpdateRows(tx, target, steps); err != nil {
		return err
	}
	for name, w := range fresh {
		if err := c.recordMTime(tx, target, name, w.rid, w.mtime); err != nil {
			return fmt.Errorf("checkout.Update: %w", err)
		}
	}
	if err := c.setUpdatedVersion(tx, target); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("checkout.Update: commit: %w", err)
	}
	return nil
}

// moveUpdateRows carries the rows of the steps over to version target
// through q: pending adds move, the target's files load, and local removals
// stay.
func (c *Checkout) moveUpdateRows(q sqlExecer, target libfossil.FslID, steps []updateStep) error {
	for _, s := range steps {
		if s.action != updateKeepAdd {
			continue
		}
		if _, err := q.Exec("UPDATE vfile SET vid = ? WHERE id = ?", int64(target), s.row.ID); err != nil {
			return fmt.Errorf("checkout.Update: keep the add of %s: %w", s.name, err)
		}
	}
	if err := c.loadVFile(q, target, true); err != nil {
		return fmt.Errorf("checkout.Update: reload vfile: %w", err)
	}
	for _, s := range steps {
		if s.row == nil || s.row.Deleted == 0 || s.target == nil {
			continue
		}
		if _, err := q.Exec("UPDATE vfile SET deleted = 1 WHERE vid = ? AND pathname = ?",
			int64(target), s.name); err != nil {
			return fmt.Errorf("checkout.Update: keep the removal of %s: %w", s.name, err)
		}
	}
	return nil
}

// setUpdatedVersion records target as the checked-out version in vvar.
func (c *Checkout) setUpdatedVersion(q sqlExecer, target libfossil.FslID) error {
	var targetUUID string
	err := c.repo.DB().QueryRow("SELECT uuid FROM blob WHERE rid = ?", int64(target)).Scan(&targetUUID)
	if err != nil {
		return fmt.Errorf("checkout.Update: query target uuid: %w", err)
	}
	if err := setVVar(q, "checkout", strconv.FormatInt(int64(target), 10)); err != nil {
		return fmt.Errorf("checkout.Update: %w", err)
	}
	if err := setVVar(q, "checkout-hash", targetUUID); err != nil {
		return fmt.Errorf("checkout.Update: %w", err)
	}
	return nil
}

// overlayScan applies a scan's findings to rows read before they were
// stored, so a dry run plans from them without writing them.
func overlayScan(rows []vfile.Row, scanned []scanWrite) {
	chnged := make(map[int64]int64, len(scanned))
	for _, w := range scanned {
		chnged[w.id] = w.chnged
	}
	for i := range rows {
		if v, ok := chnged[rows[i].ID]; ok {
			rows[i].Chnged = v
		}
	}
}

// checkUpdateTargets refuses, before anything is written, an update that
// could not finish: a file it writes or deletes where something other than
// a regular file stands or under a parent that is not a directory
// (checkMergeTarget), or a read-only file it writes. Fossil rolls back such
// an update; go-libfossil cannot roll back disk, so it refuses first.
func (c *Checkout) checkUpdateTargets(steps []updateStep) error {
	for _, s := range steps {
		switch s.action {
		case updateWrite, updateAddOver, updateMerge, updateRemove:
		default:
			continue
		}
		if err := c.checkMergeTarget(s.name); err != nil {
			return err
		}
		if s.action == updateRemove {
			continue
		}
		fullPath, err := c.safePath(s.name)
		if err != nil {
			return err
		}
		info, err := c.env.Storage.Stat(fullPath)
		if err == nil && info.Mode().Perm()&0o200 == 0 {
			return fmt.Errorf("cannot update %s: it is read-only", s.name)
		}
	}
	return nil
}

// setUpdatedModes gives every file of the target on disk the target's
// executable bit, as fossil's checkout_set_all_exe does after an update.
func (c *Checkout) setUpdatedModes(steps []updateStep) error {
	for _, s := range steps {
		if s.target == nil {
			continue
		}
		if err := c.setMergedMode(s.name, s.target.exe); err != nil {
			return err
		}
	}
	return nil
}
