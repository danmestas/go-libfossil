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
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/vfile"
	"github.com/danmestas/go-libfossil/simio"
)

// Save snapshots the current checkout state so it can be undone later.
// If pathnames is non-nil and non-empty, only those vfile entries are saved;
// otherwise all vfile rows are included. Files are read through env.Storage.
//
// A saved vfile row keeps its mtime only while it vouches for the saved
// content, that is while the file's mtime still matches it, so Undo can
// record a restored file's new mtime on it; otherwise it is saved with 0.
func Save(ckout *sql.DB, env *simio.Env, dir string, pathnames []string) error {
	if ckout == nil {
		panic("undo.Save: ckout must not be nil")
	}
	if env == nil {
		panic("undo.Save: env must not be nil")
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
		f, err := ReadFile(env.Storage, dir, name)
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
// directory dir, for SaveTx. A missing file is recorded as missing.
func ReadFile(storage simio.Storage, dir, pathname string) (File, error) {
	if storage == nil || dir == "" || pathname == "" {
		panic("undo.ReadFile: storage, dir and pathname must be set")
	}
	disk, err := readDisk(storage, filepath.Join(dir, pathname))
	if err != nil {
		return File{}, err
	}
	return File{pathname: pathname, disk: disk}, nil
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

// RestoreFiles puts files back on disk as ReadFile found them, as fossil's
// undo_rollback does for an operation that failed partway.
func RestoreFiles(env *simio.Env, dir string, files []File) error {
	if env == nil || dir == "" {
		panic("undo.RestoreFiles: env and dir must be set")
	}
	var errs []error
	for _, f := range files {
		e := undoEntry{pathname: f.pathname, oldContent: f.disk.content,
			oldExists: f.disk.exists, oldExec: f.disk.exec}
		if _, err := swapDiskFile(env, filepath.Join(dir, f.pathname), "undo", e); err != nil {
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
// executable bit from disk, a pathname saved once), and clears the saved
// vfile row's mtime unless it vouches for that content.
func saveFile(tx *sql.Tx, f File) error {
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO undo(pathname, redoflag, existsflag, isExe, isLink, content)"+
			" VALUES(?, 0, ?, ?, 0, ?)",
		f.pathname, f.disk.exists, f.disk.exec, f.disk.blob(),
	); err != nil {
		return fmt.Errorf("save %s: %w", f.pathname, err)
	}
	return unvouch(tx, f.pathname, f.disk)
}

// diskFile is a working file as read from storage.
type diskFile struct {
	exists  bool
	exec    bool
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

// readDisk reads the file at fullPath; a missing file is not an error.
func readDisk(storage simio.Storage, fullPath string) (diskFile, error) {
	data, err := storage.ReadFile(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return diskFile{}, nil
	}
	if err != nil {
		return diskFile{}, fmt.Errorf("read %s: %w", fullPath, err)
	}
	info, err := storage.Stat(fullPath)
	if err != nil {
		return diskFile{}, fmt.Errorf("stat %s: %w", fullPath, err)
	}
	return diskFile{
		exists: true, exec: info.Mode().Perm()&0o100 != 0,
		content: data, mtime: info.ModTime().Unix(),
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

// Undo restores the state saved by the most recent Save call.
// It requires undo_available == 1.
func Undo(ckout *sql.DB, env *simio.Env, dir string) error {
	if ckout == nil {
		panic("undo.Undo: ckout must not be nil")
	}
	if env == nil {
		panic("undo.Undo: env must not be nil")
	}
	if dir == "" {
		panic("undo.Undo: dir must not be empty")
	}
	return swapState(ckout, env, dir, false, nil)
}

// Redo reverses the most recent Undo.
// It requires undo_available == 2.
func Redo(ckout *sql.DB, env *simio.Env, dir string) error {
	if ckout == nil {
		panic("undo.Redo: ckout must not be nil")
	}
	if env == nil {
		panic("undo.Redo: env must not be nil")
	}
	if dir == "" {
		panic("undo.Redo: dir must not be empty")
	}
	return swapState(ckout, env, dir, true, nil)
}

// Swap is Undo, or Redo when redo is set, that also keeps vvar's
// checkout-hash naming the checked-out version, as fossil's db_set_checkout
// does: hashOf returns the hash of a check-in rid.
func Swap(ckout *sql.DB, env *simio.Env, dir string, redo bool, hashOf func(int64) (string, error)) error {
	if ckout == nil || env == nil || dir == "" {
		panic("undo.Swap: ckout, env and dir must be set")
	}
	if hashOf == nil {
		panic("undo.Swap: hashOf must not be nil")
	}
	return swapState(ckout, env, dir, redo, hashOf)
}

// swapState swaps the working files, vfile, vmerge and the checkout version
// with the saved ones. Files are read and written through env.Storage, and
// each written file's mtime is recorded on its restored row where that row
// vouched for the content when it was saved (see Save).
func swapState(
	ckout *sql.DB, env *simio.Env, dir string, redo bool, hashOf func(int64) (string, error),
) error {
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
			return fmt.Errorf("undo.%s: nothing to %s", label, label)
		}
		return fmt.Errorf("undo.%s: query undo_available: %w", label, err)
	}
	if avail != wantAvail {
		return fmt.Errorf("undo.%s: nothing to %s", label, label)
	}

	tx, err := ckout.Begin()
	if err != nil {
		return fmt.Errorf("undo.%s: begin tx: %w", label, err)
	}
	defer tx.Rollback()

	swapped, err := swapDiskFiles(tx, env, dir, label, wantFlag)
	if err != nil {
		return err
	}
	if err := swapTables(tx, label); err != nil {
		return err
	}
	for _, f := range swapped {
		if err := f.record(tx); err != nil {
			return fmt.Errorf("undo.%s: %w", label, err)
		}
	}
	if err := swapCheckout(tx, label, setAvail, hashOf); err != nil {
		return err
	}

	return tx.Commit()
}

// swappedFile is a file swapDiskFiles swapped: what it found on disk, and
// what it wrote.
type swappedFile struct {
	pathname string
	was      diskFile
	written  bool
	mtime    int64 // vfile.mtime to record for the written file
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
}

// loadUndoEntries reads the undo rows on the wantFlag side up front, so no
// cursor is held open during file I/O.
func loadUndoEntries(tx *sql.Tx, label string, wantFlag bool) ([]undoEntry, error) {
	rows, err := tx.Query(
		"SELECT rowid, pathname, content, existsflag, coalesce(isExe, 0) FROM undo"+
			" WHERE redoflag=? ORDER BY rowid",
		wantFlag)
	if err != nil {
		return nil, fmt.Errorf("undo.%s: query undo: %w", label, err)
	}
	defer rows.Close()
	var entries []undoEntry
	for rows.Next() {
		var e undoEntry
		if err := rows.Scan(&e.rowid, &e.pathname, &e.oldContent, &e.oldExists, &e.oldExec); err != nil {
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
	tx *sql.Tx, env *simio.Env, dir, label string, wantFlag bool,
) ([]swappedFile, error) {
	entries, err := loadUndoEntries(tx, label, wantFlag)
	if err != nil {
		return nil, err
	}
	swapped := make([]swappedFile, 0, len(entries))
	for _, e := range entries {
		f, err := swapDiskFile(env, filepath.Join(dir, e.pathname), label, e)
		if err != nil {
			return nil, fmt.Errorf("undo.%s: %w", label, err)
		}
		// Swap: update undo row with current content and flip redoflag.
		if _, err := tx.Exec("UPDATE undo SET content=?, existsflag=?, isExe=?, redoflag=? WHERE rowid=?",
			f.was.blob(), f.was.exists, f.was.exec, !wantFlag, e.rowid); err != nil {
			return nil, fmt.Errorf("undo.%s: update undo row: %w", label, err)
		}
		swapped = append(swapped, f)
	}
	return swapped, nil
}

// swapDiskFile puts e's saved content at fullPath, or removes the file if
// it did not exist, returning what was there before.
func swapDiskFile(env *simio.Env, fullPath, label string, e undoEntry) (swappedFile, error) {
	cur, err := readDisk(env.Storage, fullPath)
	if err != nil {
		return swappedFile{}, err
	}
	f := swappedFile{pathname: e.pathname, was: cur}
	if !e.oldExists {
		if cur.exists {
			if err := env.Storage.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return swappedFile{}, fmt.Errorf("remove %s: %w", e.pathname, err)
			}
			fmt.Printf("DELETE %s\n", e.pathname)
		}
		return f, nil
	}

	if err := env.Storage.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return swappedFile{}, fmt.Errorf("mkdir for %s: %w", e.pathname, err)
	}
	perm := os.FileMode(0o644)
	if e.oldExec {
		perm = 0o755
	}
	content := e.oldContent
	if content == nil {
		content = []byte{} // an empty stored blob scans as nil
	}
	if err := env.Storage.WriteFile(fullPath, content, perm); err != nil {
		return swappedFile{}, fmt.Errorf("write %s: %w", e.pathname, err)
	}
	if f.mtime, err = vfile.WrittenMTime(env, fullPath); err != nil {
		return swappedFile{}, err
	}
	f.written = true
	if !cur.exists {
		fmt.Printf("NEW    %s\n", e.pathname)
	} else {
		fmt.Printf("%s %s\n", labelUpper(label), e.pathname)
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

func labelUpper(s string) string {
	if s == "undo" {
		return "UNDO  "
	}
	return "REDO  "
}
