//go:build !js

// Package stash saves and restores working directory changes, storing deltas
// against baseline blobs in the checkout database (.fslckout).
package stash

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/vfile"
	"github.com/danmestas/go-libfossil/simio"
)

// Entry represents a single stash entry.
type Entry struct {
	ID      int64
	Hash    string // UUID of checkout manifest (baseline version)
	Comment string
	CTime   string
}

// EnsureTables creates the stash and stashfile tables if they don't exist.
func EnsureTables(ckout *sql.DB) error {
	if ckout == nil {
		panic("stash.EnsureTables: ckout must not be nil")
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS stash(
			stashid INTEGER PRIMARY KEY,
			hash    TEXT,
			comment TEXT,
			ctime   TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS stashfile(
			stashid   INTEGER REFERENCES stash,
			isAdded   BOOLEAN,
			isRemoved BOOLEAN,
			isExec    BOOLEAN,
			isLink    BOOLEAN,
			hash      TEXT,
			origname  TEXT,
			newname   TEXT,
			delta     BLOB,
			PRIMARY KEY(newname, stashid)
		)`,
	}
	for _, s := range stmts {
		if _, err := ckout.Exec(s); err != nil {
			return fmt.Errorf("stash.EnsureTables: %w", err)
		}
	}
	return nil
}

// nextStashID reads and increments the stash-next vvar counter.
func nextStashID(tx *sql.Tx) (int64, error) {
	var val string
	err := tx.QueryRow("SELECT value FROM vvar WHERE name='stash-next'").Scan(&val)
	if err != nil {
		if err == sql.ErrNoRows {
			// First stash: start at 1.
			if _, err := tx.Exec("INSERT INTO vvar(name,value) VALUES('stash-next','2')"); err != nil {
				return 0, fmt.Errorf("stash: init stash-next: %w", err)
			}
			return 1, nil
		}
		return 0, fmt.Errorf("stash: read stash-next: %w", err)
	}
	id, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("stash: parse stash-next %q: %w", val, err)
	}
	if _, err := tx.Exec("REPLACE INTO vvar(name,value) VALUES('stash-next',?)", strconv.FormatInt(id+1, 10)); err != nil {
		return 0, fmt.Errorf("stash: bump stash-next: %w", err)
	}
	return id, nil
}

// Scanner brings a checkout's vfile up to date with its working files, as
// fossil's vfile_check_signature does, and lists the names its checked-out
// version holds with their permissions. A *checkout.Checkout is one.
type Scanner interface {
	ScanChanges(flags checkout.ScanFlags) error
	VersionPerms() (map[string]string, error)
}

// Save stashes all changed files in the checkout, then reverts the working directory.
// It scans the checkout with scanner first, as fossil's stash_create does, so
// an edit no scan has recorded yet is stashed too.
//
// A pending merge is stashed as fossil stashes it: each changed file against
// the content last written for it (mrid), and only the stashed files are
// reverted, so vmerge is kept and the merge stays pending. A file the merge
// added is not in the checked-out version, so its revert deletes it. As
// fossil's revert does, a reverted file takes the checked-out version's
// executable and symlink bits, while the stash records the merged ones.
func Save(
	scanner Scanner, ckout *sql.DB, repoDB *sql.DB, env *simio.Env, dir string, comment string,
) error {
	if scanner == nil {
		panic("stash.Save: scanner must not be nil")
	}
	if ckout == nil {
		panic("stash.Save: ckout must not be nil")
	}
	if repoDB == nil {
		panic("stash.Save: repoDB must not be nil")
	}
	if env == nil {
		panic("stash.Save: env must not be nil")
	}
	if dir == "" {
		panic("stash.Save: dir must not be empty")
	}
	if err := scanner.ScanChanges(0); err != nil {
		return fmt.Errorf("stash.Save: %w", err)
	}
	if err := EnsureTables(ckout); err != nil {
		return err
	}
	inVersion, err := mergeVersionPerms(scanner, ckout)
	if err != nil {
		return fmt.Errorf("stash.Save: %w", err)
	}

	tx, err := ckout.Begin()
	if err != nil {
		return fmt.Errorf("stash.Save: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Get checkout hash (manifest UUID).
	var checkoutHash string
	err = tx.QueryRow("SELECT value FROM vvar WHERE name='checkout-hash'").Scan(&checkoutHash)
	if err != nil {
		// Fall back to checkout rid if checkout-hash not available.
		checkoutHash = ""
	}

	stashID, err := nextStashID(tx)
	if err != nil {
		return err
	}

	// Insert stash header, its time a Julian day as fossil's stash writes it.
	if _, err := tx.Exec("INSERT INTO stash(stashid, hash, comment, ctime) VALUES(?,?,?,julianday('now'))",
		stashID, checkoutHash, comment); err != nil {
		return fmt.Errorf("stash.Save: insert stash: %w", err)
	}

	files, err := snapshotChangedFiles(tx)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("stash.Save: no changes to stash")
	}

	disk, err := vfile.OpenDisk(env, repoDB)
	if err != nil {
		return fmt.Errorf("stash.Save: %w", err)
	}
	if err := storeAndRevertFiles(tx, repoDB, disk, dir, stashID, files, inVersion); err != nil {
		return err
	}

	return tx.Commit()
}

// mergeVersionPerms returns the names the checked-out version holds, with
// their permissions, while a merge is pending, or nil when none is: only a
// merge puts rows in vfile that the version does not hold, or bits it does
// not give them.
func mergeVersionPerms(scanner Scanner, ckout *sql.DB) (map[string]string, error) {
	var merging bool
	if err := ckout.QueryRow("SELECT EXISTS(SELECT 1 FROM vmerge)").Scan(&merging); err != nil {
		return nil, fmt.Errorf("query vmerge: %w", err)
	}
	if !merging {
		return nil, nil
	}
	return scanner.VersionPerms()
}

// snapshotChangedFiles returns the checked-out version's changed rows,
// as vfile.Classify judges them. A permission change or a rename is refused:
// Save records and restores files by current name with default permissions,
// which would drop the mode change or the rename.
func snapshotChangedFiles(tx *sql.Tx) ([]vfile.Row, error) {
	if tx == nil {
		panic("stash.snapshotChangedFiles: nil tx")
	}

	vid, err := checkoutVersion(tx)
	if err != nil {
		return nil, fmt.Errorf("stash.Save: %w", err)
	}
	rows, err := vfile.Load(tx, vid)
	if err != nil {
		return nil, fmt.Errorf("stash.Save: %w", err)
	}

	var files []vfile.Row
	for _, r := range rows {
		if vfile.Classify(r, false) == vfile.ChangeNone {
			continue
		}
		if r.ModeChanged() {
			return nil, fmt.Errorf(
				"stash.Save: %s has a pending permission change; commit or revert it first",
				r.Pathname)
		}
		if r.RenamedFrom() != "" {
			return nil, fmt.Errorf(
				"stash.Save: %s is a pending rename; commit or revert it first", r.Pathname)
		}
		files = append(files, r)
	}
	return files, nil
}

// storeAndRevertFiles records each changed file in the stash, then reverts
// it to its committed version. Files are read and written through disk, as
// fossil's stash and revert see them with RepoFILE. inVersion, set while a
// merge is pending, maps the files the checked-out version holds to their
// permissions.
func storeAndRevertFiles(
	tx *sql.Tx, repoDB *sql.DB, disk vfile.Disk, dir string, stashID int64, files []vfile.Row,
	inVersion map[string]string,
) error {
	if stashID <= 0 {
		panic("stash.storeAndRevertFiles: stashID must be positive")
	}
	ins, err := tx.Prepare(`INSERT INTO stashfile(stashid, isAdded, isRemoved, isExec, isLink,
		hash, origname, newname, delta) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("stash.Save: prepare insert: %w", err)
	}
	defer ins.Close()

	for _, f := range files {
		fullPath := filepath.Join(dir, f.Pathname)
		rec, err := stashRecord(repoDB, disk, fullPath, f)
		if err != nil {
			return err
		}
		if _, err := ins.Exec(stashID, f.IsAdded(), f.IsRemoved(), f.IsExe != 0, rec.isLink,
			nullStr(rec.baselineHash), f.Pathname, f.Pathname, rec.delta); err != nil {
			return fmt.Errorf("stash.Save: insert stashfile %s: %w", f.Pathname, err)
		}
		reverted := f
		merged := false
		if inVersion != nil {
			perm, ok := inVersion[f.Pathname]
			merged = !ok
			reverted.IsExe, reverted.IsLink = boolInt(perm == "x"), boolInt(perm == "l")
		}
		if err := revertStashedFile(tx, repoDB, disk, fullPath, reverted, merged); err != nil {
			return err
		}
	}
	return nil
}

// stashed is what the stash keeps for a changed file.
type stashed struct {
	baselineHash string // the committed artifact; empty for an added file
	delta        []byte // an added file's content, or a delta to the working content
	isLink       bool   // the working file is a symlink seen as itself
}

// stashRecord returns what the stash keeps for f, read through disk as
// fossil's stash_add_file_or_dir reads it: an added file's content with no
// baseline, a removed file's baseline hash with an empty delta, or a
// modified file's baseline hash with a delta from baseline to its content.
// The baseline is the content last written for the file (mrid), which a
// pending merge makes the merged-in artifact.
// A symlink's content is its target's name, and it is recorded as a link
// whatever vfile says, as fossil records file_islink.
func stashRecord(
	repoDB *sql.DB, disk vfile.Disk, fullPath string, f vfile.Row,
) (stashed, error) {
	info, err := disk.Stat(fullPath)
	if err != nil {
		return stashed{}, fmt.Errorf("stash.Save: %w", err)
	}
	rec := stashed{isLink: vfile.IsLink(info)}
	if f.IsAdded() {
		if rec.delta, err = disk.Read(fullPath, info); err != nil {
			return stashed{}, fmt.Errorf("stash.Save: read added %s: %w", f.Pathname, err)
		}
		return rec, nil
	}

	if err := repoDB.QueryRow(
		"SELECT uuid FROM blob WHERE rid=?", f.MRID,
	).Scan(&rec.baselineHash); err != nil {
		return stashed{}, fmt.Errorf("stash.Save: get uuid for rid=%d: %w", f.MRID, err)
	}
	if f.IsRemoved() {
		rec.delta = []byte{}
		return rec, nil
	}
	baseline, err := content.Expand(repoDB, libfossil.FslID(f.MRID))
	if err != nil {
		return stashed{}, fmt.Errorf("stash.Save: expand rid=%d: %w", f.MRID, err)
	}
	working, err := disk.Read(fullPath, info)
	if err != nil {
		return stashed{}, fmt.Errorf("stash.Save: read %s: %w", f.Pathname, err)
	}
	if rec.delta, err = delta.Create(baseline, working); err != nil {
		return stashed{}, fmt.Errorf("stash.Save: delta %s: %w", f.Pathname, err)
	}
	return rec, nil
}

// revertStashedFile undoes f's change as fossil's revert does: an added
// file, or one a pending merge added (merged), is removed from disk and
// vfile; a removed or modified file gets its committed content back, with
// f's executable and symlink bits (vfile.Disk.Write), and its row is marked
// unchanged and no longer merged, with those bits and the mtime it was
// written with.
func revertStashedFile(
	tx *sql.Tx, repoDB *sql.DB, disk vfile.Disk, fullPath string, f vfile.Row, merged bool,
) error {
	if f.IsAdded() || merged {
		if err := disk.Remove(fullPath); err != nil {
			return fmt.Errorf("stash.Save: remove added %s: %w", f.Pathname, err)
		}
		if _, err := tx.Exec("DELETE FROM vfile WHERE id=?", f.ID); err != nil {
			return fmt.Errorf("stash.Save: delete vfile %s: %w", f.Pathname, err)
		}
		return nil
	}

	baseline, err := content.Expand(repoDB, libfossil.FslID(f.RID))
	if err != nil {
		return fmt.Errorf("stash.Save: expand rid=%d for revert: %w", f.RID, err)
	}
	mtime, err := writeFile(disk, fullPath, baseline, f.IsExe != 0, f.IsLink != 0)
	if err != nil {
		return fmt.Errorf("stash.Save: %w", err)
	}
	if _, err := tx.Exec(
		"UPDATE vfile SET deleted=0, chnged=0, mrid=rid, mhash=NULL, isexe=?, islink=?, mtime=? WHERE id=?",
		f.IsExe, f.IsLink, mtime, f.ID,
	); err != nil {
		return fmt.Errorf("stash.Save: update vfile %s: %w", f.Pathname, err)
	}
	return nil
}

// writeFile writes data to fullPath through disk, a symlink to data when
// link is set, and returns the vfile.mtime to record for it.
func writeFile(disk vfile.Disk, fullPath string, data []byte, isExec, link bool) (int64, error) {
	if err := disk.Write(fullPath, data, isExec, link); err != nil {
		return 0, err
	}
	return disk.MTime(fullPath)
}

// stashedFile is one stashfile row, as Apply reads it.
type stashedFile struct {
	isAdded   bool
	isRemoved bool
	isExec    bool
	isLink    bool
	hash      sql.NullString
	name      string
	delta     []byte
}

// Apply restores stashed files to the working directory without removing the stash entry.
//
// Files are written through env's storage as fossil sees them with RepoFILE
// (vfile.Disk): a stashed symlink is made a symlink again, and nothing is
// written through one. As fossil's stash_apply does, an
// added file is added again, and a removed file is deleted from disk with its
// row left as it is, so it shows as missing. A modified file's row records
// what a scan would find: whether the written content differs from the row's
// artifact, and the mtime it was written with (vfile.WrittenMTime). A row
// based on another artifact, or in a merge, gets mtime 0 instead, so the next
// scan hashes the file.
//
// A modified file whose file on disk is no longer the stash's baseline, as
// after an edit or an update, gets the stashed change merged three ways onto
// it, as fossil's stash_apply does with merge_3way: a conflict is written
// with fossil's markers, the three inputs kept beside it, and the file is
// returned in conflicts; binary content cannot be merged and leaves the
// file empty. One that cannot be merged because a symlink is involved is
// left as it is and returned in conflicts, as fossil's reports "Cannot
// merge symlink" (see applyStashedFile).
//
// A modified file missing from disk fails the whole apply before anything
// is written, as fossil's fails ("no such file") and rolls back: so does a
// file a pending merge added, which Save deleted.
func Apply(
	ckout *sql.DB, repoDB *sql.DB, env *simio.Env, dir string, stashID int64,
) (conflicts []string, err error) {
	if ckout == nil {
		panic("stash.Apply: ckout must not be nil")
	}
	if repoDB == nil {
		panic("stash.Apply: repoDB must not be nil")
	}
	if env == nil {
		panic("stash.Apply: env must not be nil")
	}
	if dir == "" {
		panic("stash.Apply: dir must not be empty")
	}
	if stashID <= 0 {
		panic("stash.Apply: stashID must be positive")
	}
	files, err := loadStashedFiles(ckout, stashID)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("stash.Apply: stash %d not found", stashID)
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.name
	}
	if err := checkout.CheckSparsePaths(ckout, "StashApply", paths); err != nil {
		return nil, fmt.Errorf("stash.Apply: %w", err)
	}

	tx, err := ckout.Begin()
	if err != nil {
		return nil, fmt.Errorf("stash.Apply: begin tx: %w", err)
	}
	defer tx.Rollback()
	vid, err := checkoutVersion(tx)
	if err != nil {
		return nil, fmt.Errorf("stash.Apply: %w", err)
	}
	disk, err := vfile.OpenDisk(env, repoDB)
	if err != nil {
		return nil, fmt.Errorf("stash.Apply: %w", err)
	}
	if err := requireModifiedOnDisk(disk, dir, files); err != nil {
		return nil, fmt.Errorf("stash.Apply: %w", err)
	}
	for _, f := range files {
		clean, err := applyStashedFile(tx, repoDB, disk, vid, filepath.Join(dir, f.name), f)
		if err != nil {
			return nil, fmt.Errorf("stash.Apply: %w", err)
		}
		if !clean {
			conflicts = append(conflicts, f.name)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("stash.Apply: commit: %w", err)
	}
	return conflicts, nil
}

// requireModifiedOnDisk fails when a stashed modified file is missing from
// disk.
func requireModifiedOnDisk(disk vfile.Disk, dir string, files []stashedFile) error {
	for _, f := range files {
		if f.isAdded || f.isRemoved {
			continue
		}
		fullPath := filepath.Join(dir, f.name)
		info, err := disk.Stat(fullPath)
		if err != nil {
			return err
		}
		if info == nil {
			return fmt.Errorf("no such file: %s", fullPath)
		}
	}
	return nil
}

// loadStashedFiles reads stash stashID's files up front, so no cursor is
// held open while they are written and their rows updated.
func loadStashedFiles(ckout *sql.DB, stashID int64) ([]stashedFile, error) {
	rows, err := ckout.Query(`SELECT isAdded, isRemoved, isExec, isLink, hash, newname, delta
		FROM stashfile WHERE stashid=?`, stashID)
	if err != nil {
		return nil, fmt.Errorf("stash.Apply: query stashfile: %w", err)
	}
	defer rows.Close()

	var files []stashedFile
	for rows.Next() {
		var f stashedFile
		var isExec, isLink sql.NullBool
		if err := rows.Scan(
			&f.isAdded, &f.isRemoved, &isExec, &isLink, &f.hash, &f.name, &f.delta,
		); err != nil {
			return nil, fmt.Errorf("stash.Apply: scan stashfile: %w", err)
		}
		f.isExec = isExec.Bool
		f.isLink = isLink.Bool
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stash.Apply: rows iteration: %w", err)
	}
	return files, nil
}

// applyStashedFile restores one stashed file in version vid's checkout,
// writing through disk, and reports whether it did so without conflict. As
// fossil's stash_apply does, an added file is written as a plain file. A
// modified file still holding the stash's baseline, a symlink just when
// the stashed one was, gets the stashed content, a symlink as a symlink.
// Otherwise, where a symlink was stashed or is now, fossil prints "Cannot
// merge symlink" and leaves the file and its row as they are; and where
// neither is, the stashed change is merged onto the file (mergeOnto).
func applyStashedFile(
	tx *sql.Tx, repoDB *sql.DB, disk vfile.Disk, vid int64, fullPath string, f stashedFile,
) (clean bool, err error) {
	switch {
	case f.isAdded:
		if _, err := writeFile(disk, fullPath, f.delta, f.isExec, false); err != nil {
			return false, err
		}
		return true, readd(tx, vid, f)
	case f.isRemoved:
		return true, disk.Remove(fullPath)
	}

	if !f.hash.Valid {
		return false, fmt.Errorf("missing baseline hash for %s", f.name)
	}
	rid, ok := content.AvailableByUUID(repoDB, f.hash.String)
	if !ok {
		return false, fmt.Errorf("baseline blob %s not found", f.hash.String)
	}
	baseline, err := content.Expand(repoDB, rid)
	if err != nil {
		return false, fmt.Errorf("expand baseline %s: %w", f.hash.String, err)
	}
	stashed, err := delta.Apply(baseline, f.delta)
	if err != nil {
		return false, fmt.Errorf("apply delta for %s: %w", f.name, err)
	}
	info, err := disk.Stat(fullPath)
	if err != nil {
		return false, err
	}
	current, err := disk.Read(fullPath, info)
	if err != nil {
		return false, err
	}
	isNewLink := vfile.IsLink(info)
	result, clean := stashed, true
	switch {
	case f.isLink == isNewLink && bytes.Equal(current, baseline):
	case f.isLink || isNewLink:
		return false, nil
	default:
		if result, clean, err = mergeOnto(disk, fullPath, baseline, current, stashed); err != nil {
			return false, err
		}
	}
	mtime, err := writeFile(disk, fullPath, result, f.isExec, f.isLink)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`
		UPDATE vfile
		   SET chnged = CASE WHEN plain THEN ? ELSE chnged END,
		       mtime = CASE WHEN plain THEN ? ELSE 0 END
		  FROM (SELECT chnged IN (0, 1) AND NOT deleted AND mrid = rid AND rid = ? AS plain
		          FROM vfile WHERE vid = ? AND pathname = ?)
		 WHERE vid = ? AND pathname = ?`,
		!bytes.Equal(result, baseline), mtime, int64(rid), vid, f.name, vid, f.name,
	); err != nil {
		return false, fmt.Errorf("update vfile %s: %w", f.name, err)
	}
	return clean, nil
}

// mergeOnto is fossil's merge_3way with MERGE_KEEP_FILES, as stash_apply
// runs it: the change from baseline to stashed is merged onto current, the
// file at fullPath. A conflict leaves fossil's markers in the result, and
// content fossil cannot diff as text leaves the result empty; either way
// the three inputs are written beside fullPath, as NAME-baseline,
// NAME-original (current) and NAME-merge (stashed), and clean is false.
func mergeOnto(
	disk vfile.Disk, fullPath string, baseline, current, stashed []byte,
) (result []byte, clean bool, err error) {
	result = []byte{}
	if merge.IsText(baseline) && merge.IsText(current) && merge.IsText(stashed) {
		res, err := (&merge.ThreeWayText{}).Merge(nonNil(baseline), nonNil(current), nonNil(stashed))
		if err != nil {
			return nil, false, err
		}
		if res.Clean {
			return res.Content, true, nil
		}
		result = res.Content
	}
	for _, keep := range []struct {
		suffix string
		data   []byte
	}{{"baseline", baseline}, {"original", current}, {"merge", stashed}} {
		name, err := newName(disk, fullPath, keep.suffix)
		if err != nil {
			return nil, false, err
		}
		if err := disk.Write(name, keep.data, false, false); err != nil {
			return nil, false, err
		}
	}
	return result, false, nil
}

// newName is fossil's file_newname: fullPath-suffix, or the first free
// fullPath-suffix-N counting from 0.
func newName(disk vfile.Disk, fullPath, suffix string) (string, error) {
	name := fullPath + "-" + suffix
	for n := 0; n <= newNamesMax; n++ {
		info, err := disk.Stat(name)
		if err != nil {
			return "", err
		}
		if info == nil {
			return name, nil
		}
		name = fmt.Sprintf("%s-%s-%d", fullPath, suffix, n)
	}
	return "", fmt.Errorf("no free name for %s-%s", fullPath, suffix)
}

// newNamesMax bounds newName's search.
const newNamesMax = 1000

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// readd adds a stashed added file to version vid again, as fossil's
// stash_apply does through add_one_file: a removed row is restored, and a
// missing one is inserted as Manage inserts an added file.
func readd(tx *sql.Tx, vid int64, f stashedFile) error {
	var exists bool
	if err := tx.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM vfile WHERE vid=? AND pathname=?)", vid, f.name,
	).Scan(&exists); err != nil {
		return fmt.Errorf("query vfile %s: %w", f.name, err)
	}
	if exists {
		if _, err := tx.Exec(
			"UPDATE vfile SET deleted=0 WHERE vid=? AND pathname=? AND deleted", vid, f.name,
		); err != nil {
			return fmt.Errorf("restore vfile %s: %w", f.name, err)
		}
		return nil
	}
	if _, err := tx.Exec(
		"INSERT INTO vfile(vid, pathname, rid, mrid, isexe, islink, chnged) VALUES(?, ?, 0, 0, ?, 0, 1)",
		vid, f.name, f.isExec,
	); err != nil {
		return fmt.Errorf("add vfile %s: %w", f.name, err)
	}
	return nil
}

// checkoutVersion returns the checked-out version, vvar's checkout.
func checkoutVersion(tx *sql.Tx) (int64, error) {
	var text string
	if err := tx.QueryRow("SELECT value FROM vvar WHERE name='checkout'").Scan(&text); err != nil {
		return 0, fmt.Errorf("read checkout version: %w", err)
	}
	vid, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("checkout version %q: %w", text, err)
	}
	return vid, nil
}

// Pop applies the most recent stash entry and removes it, returning the
// files Apply did not merge. As fossil's stash pop does, the entry is
// removed even then.
func Pop(ckout *sql.DB, repoDB *sql.DB, env *simio.Env, dir string) (conflicts []string, err error) {
	if ckout == nil {
		panic("stash.Pop: ckout must not be nil")
	}
	if repoDB == nil {
		panic("stash.Pop: repoDB must not be nil")
	}
	if env == nil {
		panic("stash.Pop: env must not be nil")
	}
	if dir == "" {
		panic("stash.Pop: dir must not be empty")
	}
	var stashID int64
	err = ckout.QueryRow("SELECT stashid FROM stash ORDER BY stashid DESC LIMIT 1").Scan(&stashID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("stash.Pop: no stash entries")
		}
		return nil, fmt.Errorf("stash.Pop: query top stash: %w", err)
	}

	if conflicts, err = Apply(ckout, repoDB, env, dir, stashID); err != nil {
		return nil, err
	}
	return conflicts, Drop(ckout, stashID)
}

// List returns all stash entries oldest first, ordered by creation time as
// fossil's stash ls orders them, with the stash id breaking a tie.
func List(ckout *sql.DB) ([]Entry, error) {
	if ckout == nil {
		panic("stash.List: ckout must not be nil")
	}
	if err := EnsureTables(ckout); err != nil {
		return nil, err
	}

	// datetime() shows the time as fossil's stash ls does, whether the row
	// holds a Julian day or text.
	rows, err := ckout.Query("SELECT stashid, hash, comment, datetime(ctime) FROM stash ORDER BY ctime, stashid")
	if err != nil {
		return nil, fmt.Errorf("stash.List: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var h, c sql.NullString
		var ct sql.NullString
		if err := rows.Scan(&e.ID, &h, &c, &ct); err != nil {
			return nil, fmt.Errorf("stash.List: scan: %w", err)
		}
		e.Hash = h.String
		e.Comment = c.String
		e.CTime = ct.String
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Drop removes a specific stash entry and its files.
func Drop(ckout *sql.DB, stashID int64) error {
	if ckout == nil {
		panic("stash.Drop: ckout must not be nil")
	}
	if stashID <= 0 {
		panic("stash.Drop: stashID must be positive")
	}
	tx, err := ckout.Begin()
	if err != nil {
		return fmt.Errorf("stash.Drop: begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM stashfile WHERE stashid=?", stashID); err != nil {
		return fmt.Errorf("stash.Drop: delete stashfile: %w", err)
	}
	res, err := tx.Exec("DELETE FROM stash WHERE stashid=?", stashID)
	if err != nil {
		return fmt.Errorf("stash.Drop: delete stash: %w", err)
	}
	n, raErr := res.RowsAffected()
	if raErr != nil {
		return fmt.Errorf("stash.Drop: rows affected: %w", raErr)
	}
	if n == 0 {
		return fmt.Errorf("stash.Drop: stash %d not found", stashID)
	}
	return tx.Commit()
}

// Clear removes all stash entries.
func Clear(ckout *sql.DB) error {
	if ckout == nil {
		panic("stash.Clear: ckout must not be nil")
	}
	tx, err := ckout.Begin()
	if err != nil {
		return fmt.Errorf("stash.Clear: begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM stashfile"); err != nil {
		return fmt.Errorf("stash.Clear: delete stashfile: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM stash"); err != nil {
		return fmt.Errorf("stash.Clear: delete stash: %w", err)
	}
	return tx.Commit()
}

// nullStr returns a sql.NullString: valid if s is non-empty.
func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
