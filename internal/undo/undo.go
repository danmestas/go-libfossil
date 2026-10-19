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
	tx, err := ckout.Begin()
	if err != nil {
		return fmt.Errorf("undo.Save: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := createUndoTables(tx); err != nil {
		return err
	}
	names, err := savedPathnames(tx, pathnames)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := saveFile(tx, env.Storage, dir, name); err != nil {
			return err
		}
	}

	// Save current checkout vid from vvar.
	if _, err := tx.Exec("DELETE FROM vvar WHERE name='undo_checkout'"); err != nil {
		return fmt.Errorf("undo.Save: clear undo_checkout: %w", err)
	}
	if _, err := tx.Exec("INSERT INTO vvar(name,value) SELECT 'undo_checkout', value FROM vvar WHERE name='checkout'"); err != nil {
		return fmt.Errorf("undo.Save: copy checkout: %w", err)
	}

	// Mark undo available.
	if _, err := tx.Exec("REPLACE INTO vvar(name,value) VALUES('undo_available','1')"); err != nil {
		return fmt.Errorf("undo.Save: set undo_available: %w", err)
	}

	return tx.Commit()
}

// createUndoTables replaces the undo tables: an empty undo table for file
// content and copies of vfile and vmerge.
func createUndoTables(tx *sql.Tx) error {
	for _, tbl := range []string{"undo", "undo_vfile", "undo_vmerge"} {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
			return fmt.Errorf("undo.Save: drop %s: %w", tbl, err)
		}
	}
	if _, err := tx.Exec(`CREATE TABLE undo(
		pathname TEXT,
		content  BLOB,
		existsflag BOOLEAN,
		isExec   BOOLEAN,
		isLink   BOOLEAN,
		redoflag BOOLEAN DEFAULT 0
	)`); err != nil {
		return fmt.Errorf("undo.Save: create undo: %w", err)
	}
	if _, err := tx.Exec("CREATE TABLE undo_vfile AS SELECT * FROM vfile"); err != nil {
		return fmt.Errorf("undo.Save: copy vfile: %w", err)
	}
	if _, err := tx.Exec("CREATE TABLE undo_vmerge AS SELECT * FROM vmerge"); err != nil {
		return fmt.Errorf("undo.Save: copy vmerge: %w", err)
	}
	return nil
}

// savedPathnames returns the vfile pathnames Save snapshots: those named in
// pathnames, or every one when pathnames is empty. They are read up front,
// so no cursor is held open during file I/O.
func savedPathnames(tx *sql.Tx, pathnames []string) ([]string, error) {
	q := "SELECT pathname FROM vfile"
	args := make([]any, len(pathnames))
	if len(pathnames) > 0 {
		q += " WHERE pathname IN (?" + strings.Repeat(",?", len(pathnames)-1) + ")"
		for i, p := range pathnames {
			args[i] = p
		}
	}
	rows, err := tx.Query(q, args...)
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

// saveFile snapshots the file name, as on disk, into the undo table, and
// clears the saved row's mtime unless it vouches for that content.
func saveFile(tx *sql.Tx, storage simio.Storage, dir, name string) error {
	disk, err := readDisk(storage, filepath.Join(dir, name))
	if err != nil {
		return fmt.Errorf("undo.Save: %w", err)
	}
	var isExec, isLink bool
	if err := tx.QueryRow(
		"SELECT coalesce(isexe, 0), coalesce(islink, 0) FROM vfile WHERE pathname=?", name,
	).Scan(&isExec, &isLink); err != nil {
		return fmt.Errorf("undo.Save: read vfile %s: %w", name, err)
	}
	if _, err := tx.Exec(
		"INSERT INTO undo(pathname, content, existsflag, isExec, isLink) VALUES(?,?,?,?,?)",
		name, disk.blob(), disk.exists, isExec, isLink,
	); err != nil {
		return fmt.Errorf("undo.Save: insert undo row: %w", err)
	}
	return unvouch(tx, name, disk)
}

// diskFile is a working file as read from storage.
type diskFile struct {
	exists  bool
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
	return diskFile{exists: true, content: data, mtime: info.ModTime().Unix()}, nil
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
	return swapState(ckout, env, dir, false)
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
	return swapState(ckout, env, dir, true)
}

// swapState swaps the working files, vfile, vmerge and the checkout version
// with the saved ones. Files are read and written through env.Storage, and
// each written file's mtime is recorded on its restored row where that row
// vouched for the content when it was saved (see Save).
func swapState(ckout *sql.DB, env *simio.Env, dir string, redo bool) error {
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
	if err := swapCheckout(tx, label, setAvail); err != nil {
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
		"SELECT rowid, pathname, content, existsflag, coalesce(isExec, 0) FROM undo WHERE redoflag=?",
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
		if _, err := tx.Exec("UPDATE undo SET content=?, existsflag=?, redoflag=? WHERE rowid=?",
			f.was.blob(), f.was.exists, !wantFlag, e.rowid); err != nil {
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
func swapCheckout(tx *sql.Tx, label, setAvail string) error {
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

	// Update undo_available.
	if _, err := tx.Exec("REPLACE INTO vvar(name,value) VALUES('undo_available',?)", setAvail); err != nil {
		return fmt.Errorf("undo.%s: set undo_available: %w", label, err)
	}
	return nil
}

func labelUpper(s string) string {
	if s == "undo" {
		return "UNDO  "
	}
	return "REDO  "
}
