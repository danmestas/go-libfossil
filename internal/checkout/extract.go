package checkout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
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
// If opts.Force is false (default), fails if locally modified files would be overwritten.
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

	if _, err := c.LoadVFile(rid, true); err != nil {
		extractErr = fmt.Errorf("checkout.Extract: %w", err)
		return extractErr
	}

	// Collect all rows before processing to avoid holding the cursor
	// open during I/O and DB writes.
	type vfileRow struct {
		id       int64
		pathname string
		blobRid  int64
		mergeRid int64
		chnged   int64
		isexe    int64
	}
	var vfRows []vfileRow

	rows, err := c.db.Query(
		"SELECT id, pathname, rid, mrid, CAST(chnged AS INTEGER), CAST(isexe AS INTEGER) "+
			"FROM vfile WHERE vid = ?",
		int64(rid),
	)
	if err != nil {
		extractErr = fmt.Errorf("checkout.Extract: query vfile: %w", err)
		return extractErr
	}
	for rows.Next() {
		var row vfileRow
		if err := rows.Scan(
			&row.id, &row.pathname, &row.blobRid, &row.mergeRid, &row.chnged, &row.isexe,
		); err != nil {
			rows.Close()
			extractErr = fmt.Errorf(
				"checkout.Extract: scan vfile row: %w", err,
			)
			return extractErr
		}
		vfRows = append(vfRows, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		extractErr = fmt.Errorf(
			"checkout.Extract: iterate vfile rows: %w", err,
		)
		return extractErr
	}

	// If Force is false, check for locally modified files before writing.
	if !opts.Force && !opts.DryRun {
		for _, row := range vfRows {
			err := c.checkNoLocalEdit(row.pathname, row.blobRid, row.mergeRid, row.chnged)
			if err != nil {
				extractErr = err
				return extractErr
			}
		}
	}

	// Look up checkin timestamp for SetMTime.
	var checkinTime time.Time
	if opts.SetMTime && !opts.DryRun {
		var mtimeRaw any
		if err := c.repo.DB().QueryRow(
			"SELECT mtime FROM event WHERE objid = ? AND type = 'ci'",
			int64(rid),
		).Scan(&mtimeRaw); err == nil {
			checkinTime, _ = db.ScanTime(mtimeRaw)
		}
	}

	for _, row := range vfRows {
		if err := c.extractSingleFile(
			row.pathname, row.blobRid, row.isexe, opts.DryRun,
		); err != nil {
			extractErr = err
			return extractErr
		}

		if opts.SetMTime && !opts.DryRun && !checkinTime.IsZero() {
			fullPath, _ := c.safePath(row.pathname)
			if fullPath != "" {
				_ = c.env.Storage.Chtimes(fullPath, checkinTime, checkinTime)
			}
		}

		c.obs.ExtractFileCompleted(ctx, row.pathname, UpdateAdded)

		if opts.Callback != nil {
			if err := opts.Callback(row.pathname, UpdateAdded); err != nil {
				extractErr = fmt.Errorf(
					"checkout.Extract: callback for %s: %w",
					row.pathname, err,
				)
				return extractErr
			}
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

// checkNoLocalEdit returns an error if extracting blobRid over pathname
// would destroy work the user has not committed: a pending merge (mrid names
// other content than rid, or chnged holds a merge state), or a file on disk
// that differs from the artifact mrid names. A file absent from disk, or a
// row with no artifact behind it (mergeRid 0), has nothing to lose.
func (c *Checkout) checkNoLocalEdit(pathname string, blobRid, mergeRid, chnged int64) error {
	if pathname == "" {
		panic("checkout.checkNoLocalEdit: empty pathname")
	}
	if mergeRid < 0 {
		panic("checkout.checkNoLocalEdit: negative mergeRid")
	}

	if mergeRid != blobRid {
		return fmt.Errorf(
			"checkout.Extract: file %s has a pending merge; use Force to overwrite",
			pathname,
		)
	}
	if chnged > 1 {
		return fmt.Errorf(
			"checkout.Extract: file %s has a pending merge; use Force to overwrite",
			pathname,
		)
	}
	if mergeRid == 0 {
		return nil
	}

	fullPath, err := c.safePath(pathname)
	if err != nil {
		return nil // Extraction rejects the path itself, with a better error.
	}
	data, err := c.env.Storage.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checkout.Extract: read %s: %w", fullPath, err)
	}

	baseline, err := c.baselineHash(mergeRid)
	if err != nil {
		return fmt.Errorf("checkout.Extract: %s: %w", pathname, err)
	}
	if hash.ContentHash(data, baseline) != baseline {
		return fmt.Errorf(
			"checkout.Extract: file %s has local changes; use Force to overwrite",
			pathname,
		)
	}
	return nil
}
