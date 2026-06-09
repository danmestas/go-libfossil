package checkout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/libfossil/internal/blob"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/internal/manifest"
)

// LoadVFile populates vfile table with entries from the specified checkin manifest.
// If clear=true, deletes all vfile rows for OTHER versions (keeps only vid=rid).
// Returns the count of missing blobs (files whose content is not in the repo).
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) LoadVFile(rid libfossil.FslID, clear bool) (missing uint32, err error) {
	if c == nil {
		panic("checkout.LoadVFile: nil *Checkout")
	}

	// Clear other versions if requested
	if clear {
		if _, err := c.db.Exec("DELETE FROM vfile WHERE vid != ?", int64(rid)); err != nil {
			return 0, fmt.Errorf("checkout.LoadVFile: clear: %w", err)
		}
	}

	// Get file list from manifest
	files, err := manifest.ListFiles(c.repo, rid)
	if err != nil {
		return 0, fmt.Errorf("checkout.LoadVFile: %w", err)
	}

	// Insert each file into vfile
	for _, file := range files {
		// Look up blob RID
		blobRID, exists := blob.Exists(c.repo.DB(), file.UUID)
		if !exists {
			// Blob not found - increment missing count
			missing++
			// Insert with rid=0 to mark as missing
			blobRID = 0
		}

		// Determine isexe flag
		isexe := 0
		if strings.Contains(file.Perm, "x") {
			isexe = 1
		}

		// Insert vfile row (INSERT OR IGNORE handles duplicates)
		_, err := c.db.Exec(`
			INSERT OR IGNORE INTO vfile(vid, pathname, rid, mrid, mhash, isexe, islink)
			VALUES(?, ?, ?, ?, ?, ?, ?)`,
			int64(rid),
			file.Name,
			int64(blobRID),
			int64(blobRID),
			file.UUID,
			isexe,
			0, // islink - symlinks not tracked yet
		)
		if err != nil {
			return 0, fmt.Errorf("checkout.LoadVFile: insert %s: %w", file.Name, err)
		}
	}

	return missing, nil
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
	blobRid  int64
	mhash    string
	chnged   int64
	deleted  int64
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

	diskHash := hash.ContentHash(data, e.mhash)

	if diskHash != e.mhash {
		if e.chnged == 0 {
			if _, err := c.db.Exec(
				"UPDATE vfile SET chnged = 1 WHERE id = ?", e.id,
			); err != nil {
				return false, false, fmt.Errorf(
					"checkout.ScanChanges: update chnged for %s: %w",
					e.pathname, err,
				)
			}
			return true, false, nil
		}
	} else {
		if e.chnged != 0 {
			if _, err := c.db.Exec(
				"UPDATE vfile SET chnged = 0 WHERE id = ?", e.id,
			); err != nil {
				return false, false, fmt.Errorf(
					"checkout.ScanChanges: reset chnged for %s: %w",
					e.pathname, err,
				)
			}
		}
	}

	return false, false, nil
}

// ScanChanges detects modified and missing files in the checkout.
// Walks the vfile table, checks each file on disk, and updates vfile.chnged accordingly.
//
// If flags includes ScanHash, hashes file content and compares to vfile.mhash.
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

	rows, err := c.db.Query(`
		SELECT id, pathname, rid, mhash, CAST(chnged AS INTEGER), CAST(deleted AS INTEGER) FROM vfile WHERE vid = ?
	`, int64(rid))
	if err != nil {
		return fmt.Errorf("checkout.ScanChanges: query vfile: %w", err)
	}

	var entries []scanVFileEntry

	for rows.Next() {
		var e scanVFileEntry
		if err := rows.Scan(&e.id, &e.pathname, &e.blobRid, &e.mhash, &e.chnged, &e.deleted); err != nil {
			rows.Close()
			return fmt.Errorf("checkout.ScanChanges: scan vfile row: %w", err)
		}
		entries = append(entries, e)
	}
	rows.Close()

	if err := rows.Err(); err != nil {
		return fmt.Errorf("checkout.ScanChanges: iterate vfile rows: %w", err)
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
