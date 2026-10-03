package checkout

import (
	"context"
	"errors"
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

// errStopVisit ends a VisitChanges walk early once its answer is known.
var errStopVisit = errors.New("stop visit")

// checkSafeToExtract refuses an Extract that would destroy work the user has
// not committed. It follows fossil's checkout command: the current version
// must have no unsaved changes (edits, adds, removals, renames, merges), and
// no file the current version does not track may sit where the target would
// write different content. Fossil prompts in that second case; a library
// cannot, so it refuses. It runs before anything in the checkout changes.
func (c *Checkout) checkSafeToExtract(target libfossil.FslID) error {
	if target <= 0 {
		panic("checkout.checkSafeToExtract: target must be positive")
	}

	current, _, err := c.Version()
	if err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	if current > 0 {
		changed, err := c.firstUnsavedChange(current)
		if err != nil {
			return fmt.Errorf("checkout.Extract: %w", err)
		}
		if changed != "" {
			return fmt.Errorf(
				"checkout.Extract: file %s has local changes; use Force to overwrite",
				changed,
			)
		}
	}
	return c.checkNoUntrackedClobber(current, target)
}

// firstUnsavedChange scans version vid and returns the name of its first
// changed file, or "" when it has none.
func (c *Checkout) firstUnsavedChange(vid libfossil.FslID) (string, error) {
	if vid <= 0 {
		panic("checkout.firstUnsavedChange: vid must be positive")
	}

	if err := c.refreshChanged(vid); err != nil {
		return "", err
	}
	var name string
	err := c.VisitChanges(vid, false, func(e ChangeEntry) error {
		name = e.Name
		return errStopVisit
	})
	if err != nil {
		if !errors.Is(err, errStopVisit) {
			return "", err
		}
	}
	return name, nil
}

// checkNoUntrackedClobber returns an error if a file that version current
// does not track exists on disk where version target would write different
// content. current may be 0 (nothing checked out yet).
func (c *Checkout) checkNoUntrackedClobber(current, target libfossil.FslID) error {
	if current < 0 {
		panic("checkout.checkNoUntrackedClobber: negative current")
	}

	tracked, err := c.trackedPaths(current)
	if err != nil {
		return err
	}
	files, err := manifest.ListFiles(c.repo, target)
	if err != nil {
		return fmt.Errorf("checkout.Extract: %w", err)
	}
	for _, f := range files {
		if tracked[f.Name] {
			continue
		}
		fullPath, err := c.safePath(f.Name)
		if err != nil {
			continue // Extraction rejects the path itself, with a better error.
		}
		data, err := c.env.Storage.ReadFile(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("checkout.Extract: read %s: %w", fullPath, err)
		}
		if hash.ContentHash(data, f.UUID) != f.UUID {
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
