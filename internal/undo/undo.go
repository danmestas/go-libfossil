//go:build !js

// Package undo saves, restores, and redoes checkout state in a Fossil .fslckout database.
//
// The undo state machine uses a vvar entry "undo_available":
//
//	0 — nothing saved
//	1 — undo available (Save was called)
//	2 — redo available (Undo was called)
//
// Only one level of undo/redo is kept; each Save replaces any previous state.
package undo

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/vfile"
)

// Save snapshots the current checkout state so it can be undone later.
// If pathnames is non-nil and non-empty, only those vfile entries are saved;
// otherwise all vfile rows are included. Files are read through disk, as
// fossil's undo_save reads them: a symlink seen as itself is saved as one.
//
// A saved vfile row keeps its mtime only while it vouches for the saved
// content, that is while the file's mtime still matches it, so Undo can
// record a restored file's new mtime on it; otherwise it is saved with 0.
func Save(ckout *sql.DB, disk vfile.Disk, dir string, pathnames []string) error {
	if ckout == nil {
		panic("undo.Save: ckout must not be nil")
	}
	if dir == "" {
		panic("undo.Save: dir must not be empty")
	}
	names, err := savedPathnames(ckout, pathnames)
	if err != nil {
		return err
	}
	files := make([]File, 0, len(names))
	for _, name := range names {
		f, err := ReadFile(disk, dir, name)
		if err != nil {
			return fmt.Errorf("undo.Save: %w", err)
		}
		files = append(files, f)
	}
	tx, err := ckout.Begin()
	if err != nil {
		return fmt.Errorf("undo.Save: begin tx: %w", err)
	}
	defer tx.Rollback()
	if err := SaveTx(tx, files); err != nil {
		return fmt.Errorf("undo.Save: %w", err)
	}
	return tx.Commit()
}

// File is a working file as fossil's undo_save records it: its content, or
// that it does not exist, read before an operation changes it.
type File struct {
	pathname string
	disk     diskFile
}

// ReadFile reads the working file pathname, relative to the checkout
// directory dir, through disk for SaveTx. A missing file is recorded as
// missing.
func ReadFile(disk vfile.Disk, dir, pathname string) (File, error) {
	if dir == "" || pathname == "" {
		panic("undo.ReadFile: dir and pathname must be set")
	}
	d, err := readDisk(disk, filepath.Join(dir, pathname))
	if err != nil {
		return File{}, err
	}
	return File{pathname: pathname, disk: d}, nil
}

// SaveTx replaces the undo state within tx, as fossil's undo_begin and
// undo_save do: copies of vfile and vmerge as they are now, files (each
// pathname once, the first kept), and the checked-out version, with undo
// marked available. The caller runs it before the operation's own changes
// to vfile, in the same transaction.
func SaveTx(tx *sql.Tx, files []File) error {
	if tx == nil {
		panic("undo.SaveTx: tx must not be nil")
	}
	if err := createUndoTables(tx); err != nil {
		return err
	}
	for _, f := range files {
		if err := saveFile(tx, f); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("REPLACE INTO vvar(name,value)" +
		" SELECT 'undo_checkout', value FROM vvar WHERE name='checkout'"); err != nil {
		return fmt.Errorf("copy checkout: %w", err)
	}
	if _, err := tx.Exec("REPLACE INTO vvar(name,value) VALUES('undo_available','1')"); err != nil {
		return fmt.Errorf("set undo_available: %w", err)
	}
	return nil
}

// RestoreFiles puts files back through disk as ReadFile found them, as
// fossil's undo_rollback does for an operation that failed partway.
func RestoreFiles(disk vfile.Disk, dir string, files []File) error {
	if dir == "" {
		panic("undo.RestoreFiles: dir must be set")
	}
	var errs []error
	for _, f := range files {
		e := undoEntry{pathname: f.pathname, oldContent: f.disk.content,
			oldExists: f.disk.exists, oldExec: f.disk.exec, oldLink: f.disk.link}
		if _, err := swapDiskFile(disk, filepath.Join(dir, f.pathname), e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// createUndoTables replaces the undo tables, as fossil's undo_reset and
// undo_begin do: an empty undo table for file content, in fossil's schema
// so fossil's undo reads it, and copies of vfile and vmerge. A stash's undo
// tables go too, so an older stash is not brought back.
func createUndoTables(tx *sql.Tx) error {
	for _, tbl := range []string{"undo", "undo_vfile", "undo_vmerge", "undo_stash", "undo_stashfile"} {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
			return fmt.Errorf("drop %s: %w", tbl, err)
		}
	}
	if _, err := tx.Exec(`CREATE TABLE undo(
		pathname   TEXT UNIQUE,
		redoflag   BOOLEAN,
		existsflag BOOLEAN,
		isExe      BOOLEAN,
		isLink     BOOLEAN,
		content    BLOB
	)`); err != nil {
		return fmt.Errorf("create undo: %w", err)
	}
	if _, err := tx.Exec("CREATE TABLE undo_vfile AS SELECT * FROM vfile"); err != nil {
		return fmt.Errorf("copy vfile: %w", err)
	}
	if _, err := tx.Exec("CREATE TABLE undo_vmerge AS SELECT * FROM vmerge"); err != nil {
		return fmt.Errorf("copy vmerge: %w", err)
	}
	return nil
}

// querier is what savedPathnames reads vfile through.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// savedPathnames returns the vfile pathnames Save snapshots: those named in
// pathnames, or every one when pathnames is empty. They are read up front,
// so no cursor is held open during file I/O.
func savedPathnames(q querier, pathnames []string) ([]string, error) {
	query := "SELECT pathname FROM vfile"
	args := make([]any, len(pathnames))
	if len(pathnames) > 0 {
		query += " WHERE pathname IN (?" + strings.Repeat(",?", len(pathnames)-1) + ")"
		for i, p := range pathnames {
			args[i] = p
		}
	}
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("undo.Save: query vfile: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("undo.Save: scan vfile: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("undo.Save: rows iteration: %w", err)
	}
	return names, nil
}

// saveFile records f in the undo table, as fossil's undo_save does (the
// executable and symlink bits from disk, a pathname saved once), and clears the saved
// vfile row's mtime unless it vouches for that content.
func saveFile(tx *sql.Tx, f File) error {
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO undo(pathname, redoflag, existsflag, isExe, isLink, content)"+
			" VALUES(?, 0, ?, ?, ?, ?)",
		f.pathname, f.disk.exists, f.disk.exec, f.disk.link, f.disk.blob(),
	); err != nil {
		return fmt.Errorf("save %s: %w", f.pathname, err)
	}
	return unvouch(tx, f.pathname, f.disk)
}

// diskFile is a working file as read through a vfile.Disk.
type diskFile struct {
	exists  bool
	exec    bool
	link    bool // a symlink seen as itself; content is its target
	content []byte
	mtime   int64 // seconds since 1970
}

// blob is the undo table's content for d: NULL for a missing file. A nil
// []byte binds as an empty blob under some drivers (ncruces), so a missing
// file's content is bound as an untyped nil.
func (d diskFile) blob() any {
	if !d.exists {
		return nil
	}
	if d.content == nil {
		return []byte{}
	}
	return d.content
}

// readDisk reads the file at fullPath as fossil's undo_save does with
// RepoFILE: a symlink seen as itself is read as the name of its target. A
// missing file is not an error.
func readDisk(disk vfile.Disk, fullPath string) (diskFile, error) {
	info, err := disk.Stat(fullPath)
	if err != nil || info == nil {
		return diskFile{}, err
	}
	data, err := disk.Read(fullPath, info)
	if err != nil {
		return diskFile{}, err
	}
	return diskFile{
		exists: true, exec: info.Mode().IsRegular() && info.Mode().Perm()&0o100 != 0,
		link: vfile.IsLink(info), content: data, mtime: info.ModTime().Unix(),
	}, nil
}

// unvouch clears the mtime of name's saved row in undo_vfile unless it is
// the file's mtime, the one case where the row vouches for the saved content.
func unvouch(tx *sql.Tx, name string, disk diskFile) error {
	if !disk.exists {
		return nil
	}
	if _, err := tx.Exec(
		"UPDATE undo_vfile SET mtime=0 WHERE pathname=? AND coalesce(mtime, 0) != ?",
		name, disk.mtime,
	); err != nil {
		return fmt.Errorf("clear saved mtime of %s: %w", name, err)
	}
	return nil
}

// Action is what Undo or Redo did to a working file.
type Action int

const (
	Restored Action = iota + 1 // overwrote the file; fossil prints UNDO or REDO
	Created                    // wrote a file that was not there; fossil prints NEW
	Deleted                    // removed the file; fossil prints DELETE
)

// String names a.
func (a Action) String() string {
	switch a {
	case Restored:
		return "restored"
	case Created:
		return "created"
	case Deleted:
		return "deleted"
	}
	return fmt.Sprintf("Action(%d)", int(a))
}

// Change is one working file Undo or Redo changed, for a caller to report
// as fossil's undo prints it.
type Change struct {
	Pathname string
	Action   Action
}

// Undo restores the state saved by the most recent Save call, returning the
// working files it changed in the order it changed them, fossil's order.
// It requires undo_available == 1.
func Undo(ckout *sql.DB, disk vfile.Disk, dir string) ([]Change, error) {
	if ckout == nil {
		panic("undo.Undo: ckout must not be nil")
	}
	if dir == "" {
		panic("undo.Undo: dir must not be empty")
	}
	return swapState(ckout, disk, dir, false, nil)
}

// Redo reverses the most recent Undo, returning the working files it changed
// as Undo does. It requires undo_available == 2.
func Redo(ckout *sql.DB, disk vfile.Disk, dir string) ([]Change, error) {
	if ckout == nil {
		panic("undo.Redo: ckout must not be nil")
	}
	if dir == "" {
		panic("undo.Redo: dir must not be empty")
	}
	return swapState(ckout, disk, dir, true, nil)
}

// Swap is Undo, or Redo when redo is set, that also keeps vvar's
// checkout-hash naming the checked-out version, as fossil's db_set_checkout
// does: hashOf returns the hash of a check-in rid.
func Swap(
	ckout *sql.DB, disk vfile.Disk, dir string, redo bool, hashOf func(int64) (string, error),
) ([]Change, error) {
	if ckout == nil || dir == "" {
		panic("undo.Swap: ckout and dir must be set")
	}
	if hashOf == nil {
		panic("undo.Swap: hashOf must not be nil")
	}
	return swapState(ckout, disk, dir, redo, hashOf)
}

// swapState swaps the working files, vfile, vmerge and the checkout version
// with the saved ones. Files are read and written through disk, and
// each written file's mtime is recorded on its restored row where that row
// vouched for the content when it was saved (see Save).
func swapState(
	ckout *sql.DB, disk vfile.Disk, dir string, redo bool, hashOf func(int64) (string, error),
) ([]Change, error) {
	label := "undo"
	wantAvail := "1"
	setAvail := "2"
	wantFlag := false
	if redo {
		label = "redo"
		wantAvail = "2"
		setAvail = "1"
		wantFlag = true
	}

	// Check undo_available.
	var avail string
	err := ckout.QueryRow("SELECT value FROM vvar WHERE name='undo_available'").Scan(&avail)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("undo.%s: nothing to %s", label, label)
		}
		return nil, fmt.Errorf("undo.%s: query undo_available: %w", label, err)
	}
	if avail != wantAvail {
		return nil, fmt.Errorf("undo.%s: nothing to %s", label, label)
	}

	tx, err := ckout.Begin()
	if err != nil {
		return nil, fmt.Errorf("undo.%s: begin tx: %w", label, err)
	}
	defer tx.Rollback()

	swapped, err := swapDiskFiles(tx, disk, dir, label, wantFlag)
	if err != nil {
		return nil, err
	}
	if err := swapTables(tx, label); err != nil {
		return nil, err
	}
	var changes []Change
	for _, f := range swapped {
		if err := f.record(tx); err != nil {
			return nil, fmt.Errorf("undo.%s: %w", label, err)
		}
		if f.action != 0 {
			changes = append(changes, Change{Pathname: f.pathname, Action: f.action})
		}
	}
	if err := swapCheckout(tx, label, setAvail, hashOf); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("undo.%s: commit: %w", label, err)
	}
	return changes, nil
}

// swappedFile is a file swapDiskFiles swapped: what it found on disk, and
// what it wrote.
type swappedFile struct {
	pathname string
	was      diskFile
	written  bool
	mtime    int64  // vfile.mtime to record for the written file
	action   Action // what was done to the file; 0 if nothing
}

// record runs after vfile and undo_vfile are swapped. The row now saved
// keeps its mtime only if it vouched for the content swapped out, and the
// restored row, if its mtime vouched for the content written, gets the
// written file's.
func (f swappedFile) record(tx *sql.Tx) error {
	if err := unvouch(tx, f.pathname, f.was); err != nil {
		return err
	}
	if !f.written {
		return nil
	}
	if _, err := tx.Exec(
		"UPDATE vfile SET mtime=? WHERE pathname=? AND coalesce(mtime, 0) != 0",
		f.mtime, f.pathname,
	); err != nil {
		return fmt.Errorf("record mtime of %s: %w", f.pathname, err)
	}
	return nil
}

// undoEntry is one undo row swapDiskFiles restores.
type undoEntry struct {
	rowid      int64
	pathname   string
	oldContent []byte
	oldExists  bool
	oldExec    bool
	oldLink    bool
}

// loadUndoEntries reads the undo rows on the wantFlag side up front, so no
// cursor is held open during file I/O.
func loadUndoEntries(tx *sql.Tx, label string, wantFlag bool) ([]undoEntry, error) {
	rows, err := tx.Query(
		"SELECT rowid, pathname, content, existsflag, coalesce(isExe, 0), coalesce(isLink, 0)"+
			" FROM undo"+
			" WHERE redoflag=? ORDER BY rowid",
		wantFlag)
	if err != nil {
		return nil, fmt.Errorf("undo.%s: query undo: %w", label, err)
	}
	defer rows.Close()
	var entries []undoEntry
	for rows.Next() {
		var e undoEntry
		if err := rows.Scan(&e.rowid, &e.pathname, &e.oldContent, &e.oldExists, &e.oldExec, &e.oldLink); err != nil {
			return nil, fmt.Errorf("undo.%s: scan undo: %w", label, err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("undo.%s: rows iteration: %w", label, err)
	}
	return entries, nil
}

// swapDiskFiles swaps file contents between disk and the undo table rows.
func swapDiskFiles(
	tx *sql.Tx, disk vfile.Disk, dir, label string, wantFlag bool,
) ([]swappedFile, error) {
	entries, err := loadUndoEntries(tx, label, wantFlag)
	if err != nil {
		return nil, err
	}
	swapped := make([]swappedFile, 0, len(entries))
	for _, e := range entries {
		f, err := swapDiskFile(disk, filepath.Join(dir, e.pathname), e)
		if err != nil {
			return nil, fmt.Errorf("undo.%s: %w", label, err)
		}
		// Swap: update undo row with current content and flip redoflag.
		if _, err := tx.Exec(
			"UPDATE undo SET content=?, existsflag=?, isExe=?, isLink=?, redoflag=? WHERE rowid=?",
			f.was.blob(), f.was.exists, f.was.exec, f.was.link, !wantFlag, e.rowid,
		); err != nil {
			return nil, fmt.Errorf("undo.%s: update undo row: %w", label, err)
		}
		swapped = append(swapped, f)
	}
	return swapped, nil
}

// swapDiskFile puts e's saved content at fullPath through disk, or removes
// the file if it did not exist, returning what was there before. As fossil's
// undo_one does, a symlink there is deleted first and a saved symlink is
// made one again, so nothing is written through a link.
func swapDiskFile(disk vfile.Disk, fullPath string, e undoEntry) (swappedFile, error) {
	cur, err := readDisk(disk, fullPath)
	if err != nil {
		return swappedFile{}, err
	}
	f := swappedFile{pathname: e.pathname, was: cur}
	if !e.oldExists {
		if cur.exists {
			if err := disk.Remove(fullPath); err != nil {
				return swappedFile{}, err
			}
			f.action = Deleted
		}
		return f, nil
	}

	if err := disk.Write(fullPath, e.oldContent, e.oldExec, e.oldLink); err != nil {
		return swappedFile{}, err
	}
	if f.mtime, err = disk.MTime(fullPath); err != nil {
		return swappedFile{}, err
	}
	f.written = true
	f.action = Restored
	if !cur.exists {
		f.action = Created
	}
	return f, nil
}

// swapTables swaps vfile/vmerge with their undo counterparts via temp tables.
func swapTables(tx *sql.Tx, label string) error {
	for _, pair := range [][2]string{{"vfile", "undo_vfile"}, {"vmerge", "undo_vmerge"}} {
		live, saved := pair[0], pair[1]
		stmts := []string{
			"DROP TABLE IF EXISTS _tmp_swap",
			fmt.Sprintf("CREATE TABLE _tmp_swap AS SELECT * FROM %s", live),
			fmt.Sprintf("DELETE FROM %s", live),
			fmt.Sprintf("INSERT INTO %s SELECT * FROM %s", live, saved),
			fmt.Sprintf("DELETE FROM %s", saved),
			fmt.Sprintf("INSERT INTO %s SELECT * FROM _tmp_swap", saved),
			"DROP TABLE _tmp_swap",
		}
		for _, s := range stmts {
			if _, err := tx.Exec(s); err != nil {
				return fmt.Errorf("undo.%s: swap %s: %w", label, live, err)
			}
		}
	}
	return nil
}

// swapCheckout swaps the checkout vid in vvar and updates undo_available.
// With hashOf, checkout-hash follows the restored vid.
func swapCheckout(tx *sql.Tx, label, setAvail string, hashOf func(int64) (string, error)) error {
	var curCheckout, undoCheckout string
	if err := tx.QueryRow("SELECT value FROM vvar WHERE name='checkout'").Scan(&curCheckout); err != nil {
		return fmt.Errorf("undo.%s: read checkout: %w", label, err)
	}
	if err := tx.QueryRow("SELECT value FROM vvar WHERE name='undo_checkout'").Scan(&undoCheckout); err != nil {
		return fmt.Errorf("undo.%s: read undo_checkout: %w", label, err)
	}
	if _, err := tx.Exec("REPLACE INTO vvar(name,value) VALUES('checkout',?)", undoCheckout); err != nil {
		return fmt.Errorf("undo.%s: set checkout: %w", label, err)
	}
	if _, err := tx.Exec("REPLACE INTO vvar(name,value) VALUES('undo_checkout',?)", curCheckout); err != nil {
		return fmt.Errorf("undo.%s: set undo_checkout: %w", label, err)
	}
	if hashOf != nil {
		if err := setCheckoutHash(tx, undoCheckout, hashOf); err != nil {
			return fmt.Errorf("undo.%s: %w", label, err)
		}
	}

	// Update undo_available.
	if _, err := tx.Exec("REPLACE INTO vvar(name,value) VALUES('undo_available',?)", setAvail); err != nil {
		return fmt.Errorf("undo.%s: set undo_available: %w", label, err)
	}
	return nil
}

// setCheckoutHash sets vvar's checkout-hash to the hash of version vid,
// given as vvar holds it.
func setCheckoutHash(tx *sql.Tx, vid string, hashOf func(int64) (string, error)) error {
	rid, err := strconv.ParseInt(vid, 10, 64)
	if err != nil {
		return fmt.Errorf("checkout %q: %w", vid, err)
	}
	hash := ""
	if rid > 0 {
		if hash, err = hashOf(rid); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("REPLACE INTO vvar(name,value) VALUES('checkout-hash',?)", hash); err != nil {
		return fmt.Errorf("set checkout-hash: %w", err)
	}
	return nil
}
