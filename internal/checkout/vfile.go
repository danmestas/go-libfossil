package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// errMissingContent reports a version whose files' content the repository
// does not fully hold (a phantom, or a delta on one), as a partial sync
// leaves it.
var errMissingContent = errors.New("content missing")

// missingNamesMax bounds how many missing files an error names.
const missingNamesMax = 10

// Compile-time check: an error must be able to name at least one file.
const _ = uint(missingNamesMax - 1)

// LoadVFile populates the vfile table with the files of checkin rid.
// If clear=true, deletes all vfile rows for OTHER versions (keeps only vid=rid).
//
// Like fossil's checkout, it refuses a version whose content is not all in
// the repository, before writing anything: a row it cannot back with content
// would read as a pending add (rid=0 means added in fossil's vfile) and could
// never be extracted. The error wraps errMissingContent and names the files.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) LoadVFile(rid libfossil.FslID, clear bool) error {
	if c == nil {
		panic("checkout.LoadVFile: nil *Checkout")
	}

	// Resolve every file before changing anything, so a refusal leaves
	// vfile as it was.
	files, blobRIDs, err := c.resolveFiles(rid)
	if err != nil {
		return fmt.Errorf("checkout.LoadVFile: %w", err)
	}

	if clear {
		if _, err := c.db.Exec("DELETE FROM vfile WHERE vid != ?", int64(rid)); err != nil {
			return fmt.Errorf("checkout.LoadVFile: clear: %w", err)
		}
	}
	for i, file := range files {
		if blobRIDs[i] <= 0 {
			panic("checkout.LoadVFile: unresolved blob for " + file.Name)
		}
		isexe := 0
		if strings.Contains(file.Perm, "x") {
			isexe = 1
		}
		// INSERT OR IGNORE keeps an existing row for this version as it is.
		_, err := c.db.Exec(`
			INSERT OR IGNORE INTO vfile(vid, pathname, rid, mrid, isexe, islink)
			VALUES(?, ?, ?, ?, ?, 0)`,
			int64(rid), file.Name, int64(blobRIDs[i]), int64(blobRIDs[i]), isexe,
		)
		if err != nil {
			return fmt.Errorf("checkout.LoadVFile: insert %s: %w", file.Name, err)
		}
	}
	return nil
}

// resolveFiles lists the files of checkin rid with the blob each one's
// content lives in. It fails, wrapping errMissingContent, when the repository
// does not hold all of that content; callers run it before changing anything
// so that a version they cannot finish is refused up front.
func (c *Checkout) resolveFiles(
	rid libfossil.FslID,
) ([]manifest.FileEntry, []libfossil.FslID, error) {
	if rid <= 0 {
		panic("checkout.resolveFiles: rid must be positive")
	}
	if c.repo == nil {
		panic("checkout.resolveFiles: nil repo")
	}

	files, err := manifest.ListFiles(c.repo, rid)
	if err != nil {
		return nil, nil, fmt.Errorf("checkout.resolveFiles: %w", err)
	}
	blobRIDs := make([]libfossil.FslID, len(files))
	var missing []string
	for i, file := range files {
		blobRID, available := content.AvailableByUUID(c.repo.DB(), file.UUID)
		if !available {
			missing = append(missing, file.Name)
			continue
		}
		blobRIDs[i] = blobRID
	}
	if len(missing) > 0 {
		return nil, nil, missingContentError(missing)
	}
	if len(blobRIDs) != len(files) {
		panic("checkout.resolveFiles: blobRIDs out of step with files")
	}
	return files, blobRIDs, nil
}

// missingContentError names up to missingNamesMax of the missing files.
func missingContentError(missing []string) error {
	if len(missing) == 0 {
		panic("checkout.missingContentError: no missing files")
	}
	named := missing
	if len(named) > missingNamesMax {
		named = named[:missingNamesMax]
	}
	if len(named) == 0 {
		panic("checkout.missingContentError: nothing to name")
	}
	more := ""
	if len(missing) > len(named) {
		more = fmt.Sprintf(" and %d more", len(missing)-len(named))
	}
	return fmt.Errorf(
		"%w for %d file(s): %s%s",
		errMissingContent, len(missing), strings.Join(named, ", "), more,
	)
}

// UnloadVFile removes all vfile entries for the specified version.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) UnloadVFile(rid libfossil.FslID) error {
	if c == nil {
		panic("checkout.UnloadVFile: nil *Checkout")
	}

	_, err := c.db.Exec("DELETE FROM vfile WHERE vid = ?", int64(rid))
	if err != nil {
		return fmt.Errorf("checkout.UnloadVFile: %w", err)
	}

	return nil
}

// scanVFileEntry holds a single vfile row for scan processing.
type scanVFileEntry struct {
	id       int64
	pathname string
	mergeRid int64 // vfile.mrid: the artifact last written for the file, 0 if none
	chnged   int64
	deleted  int64
}

// baselineHash returns the artifact hash a file on disk is judged against:
// the uuid of vfile.mrid. mrid equals rid except while a merge is pending,
// when it names the merged-in version. This is fossil's rule (its scan joins
// blob on mrid). vfile.mhash is deliberately not read: fossil only fills it
// during a merge, so it is NULL on ordinary rows (#228).
func (c *Checkout) baselineHash(mergeRid int64) (string, error) {
	if mergeRid <= 0 {
		panic("checkout.baselineHash: mergeRid must be positive")
	}

	var uuid string
	err := c.repo.DB().QueryRow(
		"SELECT uuid FROM blob WHERE rid = ?", mergeRid,
	).Scan(&uuid)
	if err != nil {
		return "", fmt.Errorf("blob uuid for rid %d: %w", mergeRid, err)
	}
	if !hash.IsValidHash(uuid) {
		return "", fmt.Errorf("blob rid %d has malformed uuid %q", mergeRid, uuid)
	}
	return uuid, nil
}

// nextChangedState returns the vfile.chnged value fossil's own scan
// (vfile_check_signature) stores for a file that is present on disk. A row
// with no artifact behind it (mrid=0) is a pending add, which fossil writes
// with chnged=0 and the scan promotes to 1. Otherwise differs reports whether
// the content differs from the artifact mrid names: unchanged (0) and the
// clean merge outcomes (2, 4) become edited (1), an edited file whose content
// is back at its baseline returns to 0, and every other merge state is kept.
func nextChangedState(chnged int64, hasBaseline, differs bool) int64 {
	if chnged < 0 {
		panic("checkout.nextChangedState: negative chnged")
	}
	if !hasBaseline {
		if differs {
			panic("checkout.nextChangedState: differs without a baseline")
		}
		if chnged == 0 {
			return 1
		}
		return chnged
	}
	if differs {
		if chnged == 0 || chnged == 2 || chnged == 4 {
			return 1
		}
		return chnged
	}
	if chnged == 1 {
		return 0
	}
	return chnged
}

// syncChangedFlag stores the scan's verdict for e in vfile.chnged and reports
// whether the file newly became edited.
func (c *Checkout) syncChangedFlag(e scanVFileEntry, hasBaseline, differs bool) (bool, error) {
	next := nextChangedState(e.chnged, hasBaseline, differs)
	if next == e.chnged {
		return false, nil
	}
	if _, err := c.db.Exec(
		"UPDATE vfile SET chnged = ? WHERE id = ?", next, e.id,
	); err != nil {
		return false, fmt.Errorf(
			"checkout.ScanChanges: update chnged for %s: %w", e.pathname, err,
		)
	}
	return next == 1, nil
}

// scanSingleEntry checks a single vfile entry against the file on disk,
// updating vfile.chnged as needed. Returns (changed, missing) booleans.
func (c *Checkout) scanSingleEntry(
	e scanVFileEntry, flags ScanFlags,
) (changed, missing bool, err error) {
	if e.deleted != 0 {
		return false, false, nil
	}

	fullPath, err := c.safePath(e.pathname)
	if err != nil {
		return false, false, fmt.Errorf(
			"checkout.ScanChanges: path traversal in %s: %w",
			e.pathname, err,
		)
	}

	data, err := c.env.Storage.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, true, nil
		}
		return false, false, fmt.Errorf(
			"checkout.ScanChanges: read %s: %w", fullPath, err,
		)
	}

	if flags&ScanHash == 0 {
		return false, false, nil
	}

	if e.mergeRid == 0 {
		// Added and not yet committed: there is no artifact to compare against.
		changed, err = c.syncChangedFlag(e, false, false)
		return changed, false, err
	}

	baseline, err := c.baselineHash(e.mergeRid)
	if err != nil {
		return false, false, fmt.Errorf(
			"checkout.ScanChanges: %s: %w", e.pathname, err,
		)
	}
	differs := hash.ContentHash(data, baseline) != baseline

	changed, err = c.syncChangedFlag(e, true, differs)
	return changed, false, err
}

// loadScanEntries reads every vfile row of version rid. The rows are
// collected up front so no cursor is held open during file I/O and the
// per-row chnged updates.
func (c *Checkout) loadScanEntries(rid libfossil.FslID) ([]scanVFileEntry, error) {
	if rid < 0 {
		panic("checkout.loadScanEntries: negative rid")
	}

	rows, err := c.db.Query(`
		SELECT id, pathname, mrid, CAST(chnged AS INTEGER), CAST(deleted AS INTEGER)
		FROM vfile WHERE vid = ?
	`, int64(rid))
	if err != nil {
		return nil, fmt.Errorf("checkout.ScanChanges: query vfile: %w", err)
	}
	defer rows.Close()

	var entries []scanVFileEntry
	for rows.Next() {
		var e scanVFileEntry
		if err := rows.Scan(&e.id, &e.pathname, &e.mergeRid, &e.chnged, &e.deleted); err != nil {
			return nil, fmt.Errorf("checkout.ScanChanges: scan vfile row: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkout.ScanChanges: iterate vfile rows: %w", err)
	}
	return entries, nil
}

// refreshChanged brings vfile.chnged for every row of version vid up to date
// with the files on disk, by hash, without observer notifications. It is the
// part of ScanChanges that other operations need before they judge what is
// safe to do; the scan they run is their own detail, not a scan to report.
func (c *Checkout) refreshChanged(vid libfossil.FslID) error {
	if vid <= 0 {
		panic("checkout.refreshChanged: vid must be positive")
	}

	entries, err := c.loadScanEntries(vid)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, _, err := c.scanSingleEntry(e, ScanHash); err != nil {
			return err
		}
	}
	return nil
}

// ScanChanges detects modified and missing files in the checkout.
// Walks the vfile table, checks each file on disk, and updates vfile.chnged accordingly.
//
// If flags includes ScanHash, hashes file content and compares it to the
// hash of the artifact vfile.mrid names (see baselineHash).
// Otherwise, uses mtime-based detection (future enhancement).
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) ScanChanges(flags ScanFlags) error {
	if c == nil {
		panic("checkout.ScanChanges: nil *Checkout")
	}

	ctx := c.obs.ScanStarted(context.Background())

	var filesScanned, filesChanged, filesMissing, filesExtra int

	defer func() {
		c.obs.ScanCompleted(ctx, ScanEnd{
			FilesScanned: filesScanned,
			FilesChanged: filesChanged,
			FilesMissing: filesMissing,
			FilesExtra:   filesExtra,
		})
	}()

	rid, _, err := c.Version()
	if err != nil {
		return fmt.Errorf("checkout.ScanChanges: %w", err)
	}

	entries, err := c.loadScanEntries(rid)
	if err != nil {
		return err
	}

	// Build a set of tracked pathnames for extra-file detection.
	tracked := make(map[string]bool, len(entries))
	for _, e := range entries {
		tracked[e.pathname] = true
		filesScanned++

		changed, missing, scanErr := c.scanSingleEntry(e, flags)
		if scanErr != nil {
			return scanErr
		}
		if changed {
			filesChanged++
		}
		if missing {
			filesMissing++
			c.obs.Error(ctx, fmt.Errorf(
				"checkout.ScanChanges: file missing: %s", e.pathname,
			))
		}
	}

	// Walk the checkout directory to detect EXTRA files (on disk but
	// not in vfile). Errors are non-fatal: log via observer and continue.
	diskFiles, walkErr := c.walkDir(c.dir)
	if walkErr != nil {
		c.obs.Error(ctx, walkErr)
	} else {
		for relPath := range diskFiles {
			if !tracked[relPath] {
				filesExtra++
			}
		}
	}

	return nil
}

// maxWalkDepth is the maximum directory nesting depth walkDir will traverse.
const maxWalkDepth = 256

// walkDirEntry is a stack element for iterative directory traversal.
type walkDirEntry struct {
	path  string
	depth int
}

// walkDir iteratively lists all files under a directory via Storage.ReadDir.
// Returns a map of relative paths (relative to c.dir) to true.
// This helper prepares for detecting EXTRA files (files on disk not in vfile).
//
// Uses an explicit stack instead of recursion (TigerStyle: no recursion).
// Enforces a maximum traversal depth of maxWalkDepth.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) walkDir(dir string) (map[string]bool, error) {
	if c == nil {
		panic("checkout.walkDir: nil *Checkout")
	}

	result := make(map[string]bool)
	stack := []walkDirEntry{{path: dir, depth: 0}}

	for len(stack) > 0 {
		// Pop from stack
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if top.depth > maxWalkDepth {
			return nil, fmt.Errorf("walkDir: exceeded max depth %d at %s", maxWalkDepth, top.path)
		}

		entries, err := c.env.Storage.ReadDir(top.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("walkDir: read %s: %w", top.path, err)
		}

		for _, entry := range entries {
			name := entry.Name()

			// Skip checkout databases and VCS directories
			switch name {
			case ".fslckout", "_FOSSIL_", ".git", ".hg", ".svn":
				continue
			}

			fullPath := filepath.Join(top.path, name)

			if entry.IsDir() {
				stack = append(stack, walkDirEntry{path: fullPath, depth: top.depth + 1})
			} else {
				relPath, err := filepath.Rel(c.dir, fullPath)
				if err != nil {
					return nil, fmt.Errorf("walkDir: compute relative path for %s: %w", fullPath, err)
				}
				result[relPath] = true
			}
		}
	}

	return result, nil
}
