package checkout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// extractSingleFile expands a blob and writes it to the checkout directory,
// a symlink entry as fossil writes it (diskView.write), returning the
// vfile.mtime to record for it. Skips writing if dryRun is true.
func (c *Checkout) extractSingleFile(
	pathname string, blobRid int64, isexe, islink int64, dryRun bool,
) (int64, error) {
	if dryRun {
		return 0, nil
	}

	data, err := content.Expand(c.repo.DB(), libfossil.FslID(blobRid))
	if err != nil {
		return 0, fmt.Errorf("checkout.Extract: expand blob for %s: %w", pathname, err)
	}

	fullPath, err := c.safePath(pathname)
	if err != nil {
		return 0, fmt.Errorf("checkout.Extract: path traversal in %s: %w", pathname, err)
	}

	parentDir := filepath.Dir(fullPath)
	if err := c.env.Storage.MkdirAll(parentDir, 0o755); err != nil {
		return 0, fmt.Errorf("checkout.Extract: mkdir %s: %w", parentDir, err)
	}

	disk, err := c.newDiskView()
	if err != nil {
		return 0, fmt.Errorf("checkout.Extract: %w", err)
	}
	if err := disk.write(c, fullPath, data, isexe != 0, islink != 0); err != nil {
		return 0, fmt.Errorf("checkout.Extract: %w", err)
	}
	mtime, err := disk.mtime(c, fullPath)
	if err != nil {
		return 0, fmt.Errorf("checkout.Extract: %w", err)
	}
	return mtime, nil
}

// Extract writes files from the specified checkin to disk via simio.Storage.
// Populates vfile, updates vvar checkout/checkout-hash to rid.
//
// If opts.DryRun is true, skips writing files but still calls observer and callback.
// If opts.Force is false (default), refuses before changing anything when the
// current version has unsaved changes or an untracked file would be
// overwritten (see checkSafeToExtract).
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Extract(rid libfossil.FslID, opts ExtractOpts) error {
	if c == nil {
		panic("checkout.Extract: nil *Checkout")
	}

	// Start observer
	ctx := c.obs.ExtractStarted(context.Background(), ExtractStart{
		Operation: "extract",
		TargetRID: rid,
	})

	var filesWritten int
	var extractErr error
	defer func() {
		c.obs.ExtractCompleted(ctx, ExtractEnd{
			Operation:    "extract",
			TargetRID:    rid,
			FilesWritten: filesWritten,
			Err:          extractErr,
		})
	}()

	// Check before LoadVFile: it replaces the current version's rows, and a
	// refusal must leave the checkout exactly as it was.
	if !opts.Force && !opts.DryRun {
		if err := c.checkSafeToExtract(rid); err != nil {
			extractErr = err
			return extractErr
		}
	}

	// The files come from the target's manifest, so a dry run never has to
	// load them into vfile; only a real extract replaces the file list.
	vfRows, err := c.extractRows(rid)
	if err != nil {
		extractErr = err
		return extractErr
	}
	if !opts.DryRun {
		if err := c.replaceFileList(rid, opts.Force); err != nil {
			extractErr = err
			return extractErr
		}
	}

	for _, row := range vfRows {
		if err := c.extractRow(ctx, rid, row, opts); err != nil {
			extractErr = err
			return extractErr
		}
		filesWritten++
	}

	if opts.DryRun {
		return nil
	}
	if extractErr = c.finalizeExtract(rid, opts.Force); extractErr != nil {
		return extractErr
	}
	if opts.SetMTime {
		extractErr = c.setFileMTimes(rid)
	}
	return extractErr
}

// finalizeExtract looks up the blob UUID for rid and updates the vvar
// checkout/checkout-hash entries.
// It also ends a pending merge on a switch to another version or a forced
// extract, but not on a plain re-extract of the version already checked out,
// which fossil's checkout does not treat as a switch.
func (c *Checkout) finalizeExtract(rid libfossil.FslID, force bool) error {
	prior, _, err := c.Version()
	if err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	endMerge := force || prior != rid

	var uuid string
	err = c.repo.DB().QueryRow("SELECT uuid FROM blob WHERE rid = ?", int64(rid)).Scan(&uuid)
	if err != nil {
		return fmt.Errorf("checkout.Extract: query blob uuid: %w", err)
	}

	if err := setVVar(c.db, "checkout", strconv.FormatInt(int64(rid), 10)); err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	if err := setVVar(c.db, "checkout-hash", uuid); err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	if !endMerge {
		return nil
	}
	// A switch ends any pending merge, as fossil's checkout does; a stale
	// vmerge row would give the next commit a bogus merge parent.
	if _, err := c.db.Exec("DELETE FROM vmerge"); err != nil {
		return fmt.Errorf("checkout.Extract: clear vmerge: %w", err)
	}
	return nil
}

// replaceFileList loads version rid into vfile. A forced extract first drops
// every row, the current version's included, as fossil's checkout --force
// does, so no pending add, rename or removal survives it; otherwise rows the
// version already has are kept (LoadVFile).
func (c *Checkout) replaceFileList(rid libfossil.FslID, force bool) error {
	if rid <= 0 {
		panic("checkout.replaceFileList: rid must be positive")
	}
	if force {
		if _, err := c.db.Exec("DELETE FROM vfile"); err != nil {
			return fmt.Errorf("checkout.Extract: clear vfile: %w", err)
		}
	}
	if err := c.LoadVFile(rid, true); err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	return nil
}

// extractRow is one vfile row Extract writes to disk.
type extractRow struct {
	pathname string
	blobRid  int64
	isexe    int64
	islink   int64
}

// extractRows lists version rid's files the cone covers -- every file of a
// dense checkout -- with the blob each one's content is in, from its
// manifest. It refuses a version whose content is missing (resolveFiles),
// before anything is written.
func (c *Checkout) extractRows(rid libfossil.FslID) ([]extractRow, error) {
	if rid <= 0 {
		panic("checkout.extractRows: rid must be positive")
	}

	files, blobRIDs, err := c.resolveFiles(rid, c.fileFilter())
	if err != nil {
		return nil, fmt.Errorf("checkout.Extract: %w", err)
	}
	out := make([]extractRow, len(files))
	for i, f := range files {
		row := extractRow{pathname: f.Name, blobRid: int64(blobRIDs[i])}
		if strings.Contains(f.Perm, "x") {
			row.isexe = 1
		}
		if strings.Contains(f.Perm, "l") {
			row.islink = 1
		}
		out[i] = row
	}
	return out, nil
}

// extractRow writes one file of version vid, records the mtime the write
// gave it, and reports it to the observer and the caller's callback.
func (c *Checkout) extractRow(
	ctx context.Context, vid libfossil.FslID, row extractRow, opts ExtractOpts,
) error {
	mtime, err := c.extractSingleFile(row.pathname, row.blobRid, row.isexe, row.islink, opts.DryRun)
	if err != nil {
		return err
	}
	if !opts.DryRun {
		if err := c.recordMTime(c.db, vid, row.pathname, row.blobRid, mtime); err != nil {
			return fmt.Errorf("checkout.Extract: %w", err)
		}
		// The file now holds the content its row names, so an edit or
		// missing file the row recorded is gone.
		if _, err := c.db.Exec(
			"UPDATE vfile SET chnged = 0 WHERE vid = ? AND pathname = ? AND mrid = ? AND chnged = 1",
			int64(vid), row.pathname, row.blobRid,
		); err != nil {
			return fmt.Errorf("checkout.Extract: reset %s: %w", row.pathname, err)
		}
	}

	c.obs.ExtractFileCompleted(ctx, row.pathname, UpdateAdded)

	if opts.Callback != nil {
		if err := opts.Callback(row.pathname, UpdateAdded); err != nil {
			return fmt.Errorf("checkout.Extract: callback for %s: %w", row.pathname, err)
		}
	}
	return nil
}

// checkSafeToExtract refuses an Extract that would destroy work the user has
// not committed. It follows fossil's checkout command, which refuses while the
// current version has unsaved changes (edits, adds, removals, renames,
// merges) and prompts before overwriting a file it does not track; a library
// cannot prompt, so it refuses. It runs before anything in the checkout
// changes.
//
// One relaxation: an edit whose content already equals what target would
// write loses nothing, so it does not block. That matters because Create
// records the tip as checked out before any file is written, so a directory
// already holding an older version looks edited relative to the tip.
func (c *Checkout) checkSafeToExtract(target libfossil.FslID) error {
	if target <= 0 {
		panic("checkout.checkSafeToExtract: target must be positive")
	}

	current, _, err := c.Version()
	if err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	files, err := manifest.ListFilesFiltered(c.repo, target, c.fileFilter())
	if err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	targetHash := make(map[string]string, len(files))
	for _, f := range files {
		targetHash[f.Name] = f.UUID
	}

	if current > 0 {
		atRisk, err := c.firstChangeAtRisk(current, targetHash)
		if err != nil {
			return fmt.Errorf("checkout.Extract: %w", err)
		}
		if atRisk != "" {
			return fmt.Errorf(
				"checkout.Extract: file %s has local changes; use Force to overwrite",
				atRisk,
			)
		}
	}
	return c.checkNoUntrackedClobber(current, files)
}

// firstChangeAtRisk scans version vid and returns the first changed file
// whose change extracting target would lose, or "" when there is none.
func (c *Checkout) firstChangeAtRisk(
	vid libfossil.FslID, targetHash map[string]string,
) (string, error) {
	if vid <= 0 {
		panic("checkout.firstChangeAtRisk: vid must be positive")
	}
	if targetHash == nil {
		panic("checkout.firstChangeAtRisk: nil targetHash")
	}

	if err := c.refreshChanged(vid); err != nil {
		return "", err
	}
	// A pending merge is unsaved work in its own right: the vmerge record
	// would be dropped even if every merged file already matches target.
	var merging bool
	if err := c.db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM vmerge)",
	).Scan(&merging); err != nil {
		return "", fmt.Errorf("query vmerge: %w", err)
	}
	rows, err := c.managedRows(vid)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		change := vfile.Classify(r, false)
		if change == ChangeNone {
			continue
		}
		// The scan marks a missing file changed, and a missing symlink
		// entry unlinked, as fossil's does, but nothing on disk is lost by
		// writing it; Create leaves every file missing until the first
		// Extract, which plays fossil's open.
		missing, err := c.isMissing(r)
		if err != nil {
			return "", err
		}
		if missing && isEditOrMode(change) && !r.PendingMerge() {
			continue
		}
		// Only a plain edit (no merge pending, not a merge state) of a file
		// target also writes can be safe; its content decides.
		if !merging && change == ChangeModified && !r.PendingMerge() && !r.ModeChanged() {
			if uuid, ok := targetHash[r.Pathname]; ok {
				clobber, err := c.wouldClobber(r.Pathname, uuid)
				if err != nil {
					return "", err
				}
				if !clobber {
					continue
				}
			}
		}
		return r.Pathname, nil
	}
	return "", nil
}

// isEditOrMode reports a change to a file's content or mode alone: not an
// add, removal or rename.
func isEditOrMode(change FileChange) bool {
	switch change {
	case ChangeModified, ChangeExecutable, ChangeSymlink, ChangeUnexec, ChangeUnlink:
		return true
	}
	return false
}

// wouldClobber reports whether writing the artifact uuid to pathname would
// destroy content: true only when a file is there and differs from it.
func (c *Checkout) wouldClobber(pathname, uuid string) (bool, error) {
	if !hash.IsValidHash(uuid) {
		panic("checkout.wouldClobber: invalid uuid for " + pathname)
	}

	fullPath, err := c.safePath(pathname)
	if err != nil {
		return false, nil // Extraction rejects the path itself, with a better error.
	}
	data, err := c.env.Storage.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("checkout.Extract: read %s: %w", fullPath, err)
	}
	return hash.ContentHash(data, uuid) != uuid, nil
}

// checkNoUntrackedClobber returns an error if a file that version current
// does not track exists on disk where target would write different content.
// current may be 0 (nothing checked out yet).
func (c *Checkout) checkNoUntrackedClobber(
	current libfossil.FslID, files []manifest.FileEntry,
) error {
	if current < 0 {
		panic("checkout.checkNoUntrackedClobber: negative current")
	}

	tracked, err := c.trackedPaths(current)
	if err != nil {
		return err
	}
	for _, f := range files {
		if tracked[f.Name] {
			continue
		}
		clobber, err := c.wouldClobber(f.Name, f.UUID)
		if err != nil {
			return err
		}
		if clobber {
			return fmt.Errorf(
				"checkout.Extract: untracked file %s has local changes; use Force to overwrite",
				f.Name,
			)
		}
	}
	return nil
}

// trackedPaths returns the set of pathnames vfile holds for version vid.
func (c *Checkout) trackedPaths(vid libfossil.FslID) (map[string]bool, error) {
	rows, err := c.db.Query("SELECT pathname FROM vfile WHERE vid = ?", int64(vid))
	if err != nil {
		return nil, fmt.Errorf("checkout.Extract: query vfile: %w", err)
	}
	defer rows.Close()

	tracked := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("checkout.Extract: scan vfile: %w", err)
		}
		tracked[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkout.Extract: iterate vfile: %w", err)
	}
	return tracked, nil
}
