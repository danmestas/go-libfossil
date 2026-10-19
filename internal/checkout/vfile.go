package checkout

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/vfile"
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
// If clear=true, deletes all vfile rows for OTHER versions (keeps only vid=rid),
// after a new row has taken the mtime of an unchanged row it replaces
// (inheritMTimes).
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
	return c.loadVFile(c.db, rid, clear)
}

// loadVFile is LoadVFile writing through q.
func (c *Checkout) loadVFile(q sqlExecer, rid libfossil.FslID, clear bool) error {

	// Resolve every file before changing anything, so a refusal leaves
	// vfile as it was.
	files, blobRIDs, err := c.resolveFiles(rid, nil)
	if err != nil {
		return fmt.Errorf("checkout.LoadVFile: %w", err)
	}

	for i, file := range files {
		if blobRIDs[i] <= 0 {
			panic("checkout.LoadVFile: unresolved blob for " + file.Name)
		}
		isexe, islink := 0, 0
		if strings.Contains(file.Perm, "x") {
			isexe = 1
		}
		if strings.Contains(file.Perm, "l") {
			islink = 1
		}
		// INSERT OR IGNORE keeps an existing row for this version as it is.
		_, err := q.Exec(`
			INSERT OR IGNORE INTO vfile(vid, pathname, rid, mrid, isexe, islink)
			VALUES(?, ?, ?, ?, ?, ?)`,
			int64(rid), file.Name, int64(blobRIDs[i]), int64(blobRIDs[i]), isexe, islink,
		)
		if err != nil {
			return fmt.Errorf("checkout.LoadVFile: insert %s: %w", file.Name, err)
		}
	}
	if !clear {
		return nil
	}
	if err := c.inheritMTimes(q, rid); err != nil {
		return fmt.Errorf("checkout.LoadVFile: %w", err)
	}
	if _, err := q.Exec("DELETE FROM vfile WHERE vid != ?", int64(rid)); err != nil {
		return fmt.Errorf("checkout.LoadVFile: clear: %w", err)
	}
	return nil
}

// resolveFiles lists the files of checkin rid that filter covers (all of
// them for a nil filter) with the blob each one's content lives in. It
// fails, wrapping errMissingContent, when the repository does not hold all
// of that content; callers run it before changing anything so that a
// version they cannot finish is refused up front.
func (c *Checkout) resolveFiles(
	rid libfossil.FslID, filter manifest.FileFilter,
) ([]manifest.FileEntry, []libfossil.FslID, error) {
	if rid <= 0 {
		panic("checkout.resolveFiles: rid must be positive")
	}
	if c.repo == nil {
		panic("checkout.resolveFiles: nil repo")
	}

	files, err := manifest.ListFilesFiltered(c.repo, rid, filter)
	if err != nil {
		return nil, nil, fmt.Errorf("checkout.resolveFiles: %w", err)
	}
	blobRIDs := make([]libfossil.FslID, len(files))
	var missing []string
	for i, file := range files {
		// A database fault must not read as missing content: Create
		// tolerates missing content, and would swallow the fault with it.
		blobRID, available, err := content.CheckAvailableByUUID(c.repo.DB(), file.UUID)
		if err != nil {
			return nil, nil, fmt.Errorf("checkout.resolveFiles: %s: %w", file.Name, err)
		}
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
	mtime    int64 // vfile.mtime: seconds since 1970, 0 when not known
	isexe    int64
	islink   int64
}

// baseline returns the hash and size of the artifact a file on disk is
// judged against: the blob vfile.mrid names. mrid equals rid except while a
// merge is pending, when it names the merged-in version. This is fossil's
// rule (its scan joins blob on mrid). vfile.mhash is deliberately not read:
// fossil only fills it during a merge, so it is NULL on ordinary rows (#228).
func (c *Checkout) baseline(mergeRid int64) (string, int64, error) {
	if mergeRid <= 0 {
		panic("checkout.baseline: mergeRid must be positive")
	}

	var uuid string
	var size int64
	err := c.repo.DB().QueryRow(
		"SELECT uuid, size FROM blob WHERE rid = ?", mergeRid,
	).Scan(&uuid, &size)
	if err != nil {
		return "", 0, fmt.Errorf("blob for rid %d: %w", mergeRid, err)
	}
	if !hash.IsValidHash(uuid) {
		return "", 0, fmt.Errorf("blob rid %d has malformed uuid %q", mergeRid, uuid)
	}
	return uuid, size, nil
}

// scanFacts is what fossil's scan knows of a file before it hashes it.
type scanFacts struct {
	hasBaseline  bool // vfile.mrid names an artifact; a pending add has none
	deleted      bool // removed with fossil rm
	notFile      bool // something other than a file or symlink is at the path
	sizeDiffers  bool // the size is not the baseline's; a missing file's never is
	mtimeTrusted bool // the mtime is the one recorded, and may be trusted
}

// nextChangedState is the content half of fossil's vfile_check_signature:
// the vfile.chnged value its scan stores before the permission half (see
// nextPermState). A pending add or removal recorded with chnged=0 is
// promoted to 1, as is anything that is not a file. A size other than the
// artifact's, a missing file's included, is an edit outright. A file
// believed edited (1) is hashed, and returns to 0 when its content is back at
// the baseline. An unchanged file (0) or a clean merge outcome (2, 4) is
// hashed only when its mtime cannot be trusted, and becomes 1 when its
// content differs. Every other state is kept: the remaining merge outcomes
// (3, 5) and the mode changes (6 through 9). differs hashes the file; it is
// called only when the verdict needs it.
func nextChangedState(
	chnged int64, f scanFacts, differs func() (bool, error),
) (int64, error) {
	if chnged < 0 {
		panic("checkout.nextChangedState: negative chnged")
	}
	if differs == nil {
		panic("checkout.nextChangedState: nil differs")
	}
	if chnged == 0 && (f.deleted || !f.hasBaseline) {
		chnged = 1
	} else if f.notFile {
		chnged = 1
	}
	unchanged := chnged == 0 || chnged == 2 || chnged == 4
	switch {
	case f.sizeDiffers:
		return 1, nil
	case chnged == 1 && f.hasBaseline && !f.deleted:
		d, err := differs()
		if err != nil || d {
			return 1, err
		}
		return 0, nil
	case unchanged && !f.mtimeTrusted:
		if !f.hasBaseline {
			return 1, nil // nothing to verify against, as fossil finds
		}
		d, err := differs()
		if err != nil || d {
			return 1, err
		}
		return chnged, nil
	}
	return chnged, nil
}

// scanPolicy is how a scan decides whether a file needs hashing.
type scanPolicy struct {
	trustMTime bool     // fossil's useMtime: no ScanHash and mtime-changes on
	now        int64    // the scan's time; see vfile.RecordableMTime
	disk       diskView // how files and their permissions are seen
	enotfile   bool     // ScanENotFile: refuse something that is not a file
}

// newScanPolicy reads the policy for flags: a hash scan (fossil's --hash)
// never trusts mtimes, and neither does any scan while the repository's
// mtime-changes setting is off.
func (c *Checkout) newScanPolicy(flags ScanFlags) (scanPolicy, error) {
	p := scanPolicy{now: c.env.Clock.Now().Unix(), enotfile: flags&ScanENotFile != 0}
	disk, err := c.newDiskView()
	if err != nil {
		return p, err
	}
	p.disk = disk
	if flags&ScanHash != 0 {
		return p, nil
	}
	on, err := c.boolSetting("mtime-changes", true)
	if err != nil {
		return p, err
	}
	p.trustMTime = on
	return p, nil
}

// boolSetting reads a boolean fossil setting, dflt when unset, as
// db_get_boolean reads it from the repository's config table. Fossil's
// db_get falls back to global_config in ~/.fossil (`fossil settings
// mtime-changes off --global`); go-libfossil reads no global fossil settings,
// so a global value is not seen here.
func (c *Checkout) boolSetting(name string, dflt bool) (bool, error) {
	var value sql.NullString
	err := c.repo.DB().QueryRow(
		"SELECT value FROM config WHERE name = ?", name,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !value.Valid) {
		return dflt, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	return settingBoolean(value.String, dflt), nil
}

// settingBoolean is fossil's db_get_boolean on a stored value: is_truth
// ("on", "yes", "true" in any case, or a nonzero C atoi) gives true, is_false
// ("off", "no", "false", "0" in any case) gives false, anything else dflt.
func settingBoolean(value string, dflt bool) bool {
	switch strings.ToLower(value) {
	case "on", "yes", "true":
		return true
	case "off", "no", "false", "0":
		return false
	}
	digits := strings.TrimLeft(value, " \t\n\v\f\r")
	if digits != "" && (digits[0] == '+' || digits[0] == '-') {
		digits = digits[1:]
	}
	digits = strings.TrimLeft(digits, "0")
	if digits != "" && digits[0] >= '1' && digits[0] <= '9' {
		return true // a nonzero atoi
	}
	return dflt
}

// fileDiffers reports whether the file at fullPath, seen through v with
// info from v.stat, hashes to other than uuid. Something that is not a file
// or symlink, or nothing at all, has no content that could match.
func (c *Checkout) fileDiffers(
	v diskView, fullPath string, info os.FileInfo, uuid string,
) (bool, error) {
	if !isFileOrLink(info) {
		return true, nil
	}
	data, err := v.read(c, fullPath, info)
	if err != nil {
		return false, err
	}
	return hash.ContentHash(data, uuid) != uuid, nil
}

// scanWrite is one vfile row a scan updates.
type scanWrite struct {
	id, mtime, chnged int64
}

// storeScan stores a scan's updates in one transaction, as fossil's scan
// runs in one.
func (c *Checkout) storeScan(writes []scanWrite) (err error) {
	if len(writes) == 0 {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("checkout.ScanChanges: begin: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	for _, w := range writes {
		if _, err := tx.Exec(
			"UPDATE vfile SET mtime = ?, chnged = ? WHERE id = ?", w.mtime, w.chnged, w.id,
		); err != nil {
			return fmt.Errorf("checkout.ScanChanges: update vfile row %d: %w", w.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("checkout.ScanChanges: commit: %w", err)
	}
	return nil
}

// scanSingleEntry checks a single vfile entry against the file on disk the
// way fossil's vfile_check_signature does, appending to writes the update
// that stores the verdict in vfile.chnged and the file's mtime in
// vfile.mtime (-1 for a missing file, as fossil stores). Returns (changed,
// missing): changed when a file present on disk newly became edited or had
// its mode changed; missing when nothing, or something other than a file or
// symlink, is at the path of a file not removed.
func (c *Checkout) scanSingleEntry(
	e scanVFileEntry, p scanPolicy, writes *[]scanWrite,
) (changed, missing bool, err error) {
	fullPath, err := c.safePath(e.pathname)
	if err != nil {
		return false, false, fmt.Errorf(
			"checkout.ScanChanges: path traversal in %s: %w",
			e.pathname, err,
		)
	}
	info, err := p.disk.stat(c, fullPath)
	if err != nil {
		return false, false, fmt.Errorf("checkout.ScanChanges: %w", err)
	}
	// Fossil's else-if: a pending add or removal still recorded with
	// chnged=0 is promoted to changed and never checked for being a file.
	promoted := e.chnged == 0 && (e.deleted != 0 || e.mergeRid == 0)
	if p.enotfile && !promoted && info != nil && !isFileOrLink(info) {
		return false, false, fmt.Errorf("checkout.ScanChanges: not a regular file: %s", e.pathname)
	}
	next, err := c.scanVerdict(e, p, fullPath, info)
	if err != nil {
		return false, false, fmt.Errorf("checkout.ScanChanges: %s: %w", e.pathname, err)
	}
	mtime := int64(-1)
	if info != nil {
		mtime = vfile.RecordableMTime(info.ModTime(), p.now)
	}
	missing = e.deleted == 0 && !isFileOrLink(info)
	if next == e.chnged && mtime == e.mtime {
		return false, missing, nil
	}
	*writes = append(*writes, scanWrite{id: e.id, mtime: mtime, chnged: next})
	changed = !missing && e.deleted == 0 && next != e.chnged && (next == 1 || next >= 6)
	return changed, missing, nil
}

// scanVerdict is the vfile.chnged value fossil's scan stores for e, given
// info from p.disk.stat (nil for a missing file): the content half, then,
// where permissions are read from disk, the permission half.
func (c *Checkout) scanVerdict(
	e scanVFileEntry, p scanPolicy, fullPath string, info os.FileInfo,
) (int64, error) {
	uuid, size := "", int64(0)
	if e.mergeRid > 0 {
		var err error
		if uuid, size, err = c.baseline(e.mergeRid); err != nil {
			return 0, err
		}
	}
	f := scanFacts{
		hasBaseline: e.mergeRid > 0,
		deleted:     e.deleted != 0,
		notFile:     info != nil && !isFileOrLink(info),
		sizeDiffers: info == nil || info.Size() != size,
	}
	// A recorded 0 means not known, even for a file dated 1970.
	f.mtimeTrusted = p.trustMTime && info != nil && e.mtime != 0 &&
		info.ModTime().Unix() == e.mtime
	next, err := nextChangedState(e.chnged, f, func() (bool, error) {
		return c.fileDiffers(p.disk, fullPath, info, uuid)
	})
	if err != nil || p.disk.links == nil {
		return next, err
	}
	orig := recordedPerm(e.isexe, e.islink)
	return nextPermState(next, orig, p.disk.perm(info, orig)), nil
}

// loadScanEntries reads every vfile row of version rid the cone covers. The rows are
// collected up front so no cursor is held open during file I/O and the
// per-row updates. Fossil leaves mtime NULL on a row it adds.
func (c *Checkout) loadScanEntries(rid libfossil.FslID) ([]scanVFileEntry, error) {
	if rid < 0 {
		panic("checkout.loadScanEntries: negative rid")
	}

	rows, err := c.db.Query(`
		SELECT id, pathname, mrid, CAST(chnged AS INTEGER), CAST(deleted AS INTEGER),
		       CAST(coalesce(mtime, 0) AS INTEGER),
		       CAST(coalesce(isexe, 0) AS INTEGER), CAST(coalesce(islink, 0) AS INTEGER)
		FROM vfile WHERE vid = ?
	`, int64(rid))
	if err != nil {
		return nil, fmt.Errorf("checkout.ScanChanges: query vfile: %w", err)
	}
	defer rows.Close()

	var entries []scanVFileEntry
	for rows.Next() {
		var e scanVFileEntry
		if err := rows.Scan(
			&e.id, &e.pathname, &e.mergeRid, &e.chnged, &e.deleted, &e.mtime,
			&e.isexe, &e.islink,
		); err != nil {
			return nil, fmt.Errorf("checkout.ScanChanges: scan vfile row: %w", err)
		}
		// An uncovered file is not on disk by design: it is never
		// scanned, so it is never missing (§4.1).
		if c.covers(e.pathname) {
			entries = append(entries, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkout.ScanChanges: iterate vfile rows: %w", err)
	}
	return entries, nil
}

// refreshChanged brings vfile.chnged for every row of version vid up to date
// with the files on disk, as fossil's default scan does, without observer
// notifications. It is the part of ScanChanges that other operations need
// before they judge what is safe to do; the scan they run is their own
// detail, not a scan to report.
func (c *Checkout) refreshChanged(vid libfossil.FslID) error {
	if vid <= 0 {
		panic("checkout.refreshChanged: vid must be positive")
	}

	writes, err := c.scanVersion(vid)
	if err != nil {
		return err
	}
	return c.storeScan(writes)
}

// scanVersion runs fossil's default scan over version vid and returns the
// vfile updates it found, without storing them.
func (c *Checkout) scanVersion(vid libfossil.FslID) ([]scanWrite, error) {
	if vid <= 0 {
		panic("checkout.scanVersion: vid must be positive")
	}
	p, err := c.newScanPolicy(0)
	if err != nil {
		return nil, err
	}
	entries, err := c.loadScanEntries(vid)
	if err != nil {
		return nil, err
	}
	var writes []scanWrite
	for _, e := range entries {
		if _, _, err := c.scanSingleEntry(e, p, &writes); err != nil {
			return nil, err
		}
	}
	return writes, nil
}

// ScanChanges detects modified and missing files in the checkout.
// Walks the vfile table, checks each file on disk, and updates vfile.chnged accordingly.
//
// As fossil's scan, a file whose size and mtime match what vfile records is
// taken as unchanged without reading it, unless flags includes ScanHash
// (fossil's --hash) or the mtime-changes setting is off; any other file is
// hashed and compared with the artifact vfile.mrid names (see baseline).
// With ScanENotFile, as fossil's CKSIG_ENOTFILE, something other than a file
// or symlink where a tracked file belongs fails the scan, storing nothing.
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

	p, err := c.newScanPolicy(flags)
	if err != nil {
		return fmt.Errorf("checkout.ScanChanges: %w", err)
	}
	entries, err := c.loadScanEntries(rid)
	if err != nil {
		return err
	}

	// Build a set of tracked pathnames for extra-file detection.
	tracked := make(map[string]bool, len(entries))
	var writes []scanWrite
	for _, e := range entries {
		tracked[e.pathname] = true
		filesScanned++

		changed, missing, scanErr := c.scanSingleEntry(e, p, &writes)
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
	if err := c.storeScan(writes); err != nil {
		return err
	}

	filesExtra = c.countExtra(ctx, tracked)
	return nil
}

// countExtra walks the checkout directory to count EXTRA files (on disk but
// not in vfile). Errors are non-fatal: logged via observer, counting none.
func (c *Checkout) countExtra(ctx context.Context, tracked map[string]bool) int {
	diskFiles, walkErr := c.walkDir(c.dir)
	if walkErr != nil {
		c.obs.Error(ctx, walkErr)
		return 0
	}
	extra := 0
	for relPath := range diskFiles {
		if !tracked[relPath] {
			extra++
		}
	}
	return extra
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
			relPath, err := filepath.Rel(c.dir, fullPath)
			if err != nil {
				return nil, fmt.Errorf("walkDir: compute relative path for %s: %w", fullPath, err)
			}

			// A sparse checkout neither walks a subtree its cone cannot
			// reach nor reports an uncovered file: an uncovered path is
			// not untracked (draft-fossil-sparse-cone-00 §4.1, §4.5).
			if entry.IsDir() {
				if c.cone == nil || c.cone.IntersectsSubtree(filepath.ToSlash(relPath)) {
					stack = append(stack, walkDirEntry{path: fullPath, depth: top.depth + 1})
				}
			} else if c.covers(filepath.ToSlash(relPath)) {
				result[relPath] = true
			}
		}
	}

	return result, nil
}
