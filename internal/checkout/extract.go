package checkout

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// extractSingleFile expands a blob and writes it to the checkout directory.
// Skips writing if dryRun is true.
func (c *Checkout) extractSingleFile(
	pathname string, blobRid int64, isexe int64, dryRun bool,
) error {
	if dryRun {
		return nil
	}

	data, err := content.Expand(c.repo.DB(), libfossil.FslID(blobRid))
	if err != nil {
		return fmt.Errorf("checkout.Extract: expand blob for %s: %w", pathname, err)
	}

	fullPath, err := c.safePath(pathname)
	if err != nil {
		return fmt.Errorf("checkout.Extract: path traversal in %s: %w", pathname, err)
	}

	parentDir := filepath.Dir(fullPath)
	if err := c.env.Storage.MkdirAll(parentDir, 0o755); err != nil {
		return fmt.Errorf("checkout.Extract: mkdir %s: %w", parentDir, err)
	}

	perm := os.FileMode(0o644)
	if isexe != 0 {
		perm = 0o755
	}

	if err := c.env.Storage.WriteFile(fullPath, data, perm); err != nil {
		return fmt.Errorf("checkout.Extract: write %s: %w", fullPath, err)
	}

	return nil
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

	if _, err := c.LoadVFile(rid, true); err != nil {
		extractErr = fmt.Errorf("checkout.Extract: %w", err)
		return extractErr
	}

	vfRows, err := c.extractRows(rid)
	if err != nil {
		extractErr = err
		return extractErr
	}

	var mtime time.Time
	if opts.SetMTime && !opts.DryRun {
		mtime = c.checkinTime(rid)
	}

	for _, row := range vfRows {
		if err := c.extractRow(ctx, row, opts, mtime); err != nil {
			extractErr = err
			return extractErr
		}
		filesWritten++
	}

	// Finalize: look up UUID and update vvar
	extractErr = c.finalizeExtract(rid)
	return extractErr
}

// finalizeExtract looks up the blob UUID for rid and updates the vvar
// checkout/checkout-hash entries.
func (c *Checkout) finalizeExtract(rid libfossil.FslID) error {
	var uuid string
	err := c.repo.DB().QueryRow("SELECT uuid FROM blob WHERE rid = ?", int64(rid)).Scan(&uuid)
	if err != nil {
		return fmt.Errorf("checkout.Extract: query blob uuid: %w", err)
	}

	if err := setVVar(c.db, "checkout", strconv.FormatInt(int64(rid), 10)); err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	if err := setVVar(c.db, "checkout-hash", uuid); err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	return nil
}

// extractRow is one vfile row Extract writes to disk.
type extractRow struct {
	pathname string
	blobRid  int64
	isexe    int64
}

// extractRows reads version rid's vfile rows. They are collected up front so
// no cursor is held open during file I/O and DB writes.
func (c *Checkout) extractRows(rid libfossil.FslID) ([]extractRow, error) {
	if rid <= 0 {
		panic("checkout.extractRows: rid must be positive")
	}

	rows, err := c.db.Query(
		"SELECT pathname, rid, CAST(isexe AS INTEGER) FROM vfile WHERE vid = ?",
		int64(rid),
	)
	if err != nil {
		return nil, fmt.Errorf("checkout.Extract: query vfile: %w", err)
	}
	defer rows.Close()

	var out []extractRow
	for rows.Next() {
		var row extractRow
		if err := rows.Scan(&row.pathname, &row.blobRid, &row.isexe); err != nil {
			return nil, fmt.Errorf("checkout.Extract: scan vfile row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkout.Extract: iterate vfile rows: %w", err)
	}
	return out, nil
}

// checkinTime returns the check-in time of rid, or the zero time when it
// cannot be read. SetMTime is best-effort: a missing event row leaves files
// with the time they were written.
func (c *Checkout) checkinTime(rid libfossil.FslID) time.Time {
	var mtimeRaw any
	err := c.repo.DB().QueryRow(
		"SELECT mtime FROM event WHERE objid = ? AND type = 'ci'", int64(rid),
	).Scan(&mtimeRaw)
	if err != nil {
		return time.Time{}
	}
	t, ok := db.ScanTime(mtimeRaw)
	if !ok {
		return time.Time{}
	}
	return t
}

// extractRow writes one file, applies mtime when it is set, and reports the
// file to the observer and the caller's callback.
func (c *Checkout) extractRow(
	ctx context.Context, row extractRow, opts ExtractOpts, mtime time.Time,
) error {
	if err := c.extractSingleFile(
		row.pathname, row.blobRid, row.isexe, opts.DryRun,
	); err != nil {
		return err
	}

	if !mtime.IsZero() {
		// Best-effort, as in fossil: a file whose mtime cannot be set is
		// still correctly extracted.
		if fullPath, err := c.safePath(row.pathname); err == nil {
			_ = c.env.Storage.Chtimes(fullPath, mtime, mtime)
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
	files, err := manifest.ListFiles(c.repo, target)
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

// changeRow is the part of a vfile row firstChangeAtRisk judges.
type changeRow struct {
	pathname string
	origname sql.NullString
	chnged   int64
	deleted  int64
	rid      int64
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
	rows, err := c.changeRows(vid)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		change := classifyChange(r.pathname, r.origname, r.chnged, r.deleted, r.rid)
		if change == ChangeNone {
			continue
		}
		// Only a plain edit (not a merge state) of a file target also
		// writes can be safe; its content decides.
		if change == ChangeModified && r.chnged == 1 {
			if uuid, ok := targetHash[r.pathname]; ok {
				clobber, err := c.wouldClobber(r.pathname, uuid)
				if err != nil {
					return "", err
				}
				if !clobber {
					continue
				}
			}
		}
		return r.pathname, nil
	}
	return "", nil
}

// changeRows reads the rows of version vid that classifyChange needs. They
// are collected up front so no cursor is held open while files are read.
func (c *Checkout) changeRows(vid libfossil.FslID) ([]changeRow, error) {
	rows, err := c.db.Query(`
		SELECT pathname, origname, CAST(chnged AS INTEGER), CAST(deleted AS INTEGER), rid
		FROM vfile WHERE vid = ?`, int64(vid))
	if err != nil {
		return nil, fmt.Errorf("query vfile: %w", err)
	}
	defer rows.Close()

	var out []changeRow
	for rows.Next() {
		var r changeRow
		if err := rows.Scan(&r.pathname, &r.origname, &r.chnged, &r.deleted, &r.rid); err != nil {
			return nil, fmt.Errorf("scan vfile: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vfile: %w", err)
	}
	return out, nil
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
