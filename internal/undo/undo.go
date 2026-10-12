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
)

// Save snapshots the current checkout state so it can be undone later.
// If pathnames is non-nil and non-empty, only those vfile entries are saved;
// otherwise all vfile rows are included.
func Save(ckout *sql.DB, dir string, pathnames []string) error {
	if ckout == nil {
		panic("undo.Save: ckout must not be nil")
	}
	if dir == "" {
		panic("undo.Save: dir must not be empty")
	}
	tx, err := ckout.Begin()
	if err != nil {
		return fmt.Errorf("undo.Save: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Drop old undo tables if they exist.
	for _, tbl := range []string{"undo", "undo_vfile", "undo_vmerge"} {
		if _, err := tx.Exec("DROP TABLE IF EXISTS " + tbl); err != nil {
			return fmt.Errorf("undo.Save: drop %s: %w", tbl, err)
		}
	}

	// Create undo table for file content snapshots.
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

	// Copy vfile and vmerge.
	if _, err := tx.Exec("CREATE TABLE undo_vfile AS SELECT * FROM vfile"); err != nil {
		return fmt.Errorf("undo.Save: copy vfile: %w", err)
	}
	if _, err := tx.Exec("CREATE TABLE undo_vmerge AS SELECT * FROM vmerge"); err != nil {
		return fmt.Errorf("undo.Save: copy vmerge: %w", err)
	}

	// Snapshot file contents from disk.
	var rows *sql.Rows
	if len(pathnames) > 0 {
		// Build placeholders.
		q := "SELECT pathname, isexe, islink FROM vfile WHERE pathname IN ("
		args := make([]any, len(pathnames))
		for i, p := range pathnames {
			if i > 0 {
				q += ","
			}
			q += "?"
			args[i] = p
		}
		q += ")"
		rows, err = tx.Query(q, args...)
	} else {
		rows, err = tx.Query("SELECT pathname, isexe, islink FROM vfile")
	}
	if err != nil {
		return fmt.Errorf("undo.Save: query vfile: %w", err)
	}
	defer rows.Close()

	ins, err := tx.Prepare("INSERT INTO undo(pathname, content, existsflag, isExec, isLink) VALUES(?,?,?,?,?)")
	if err != nil {
		return fmt.Errorf("undo.Save: prepare insert: %w", err)
	}
	defer ins.Close()

	for rows.Next() {
		var pathname string
		var isExec, isLink bool
		if err := rows.Scan(&pathname, &isExec, &isLink); err != nil {
			return fmt.Errorf("undo.Save: scan vfile: %w", err)
		}
		fullPath := filepath.Join(dir, pathname)
		data, readErr := os.ReadFile(fullPath)
		exists := true
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				exists = false
				data = nil
			} else {
				return fmt.Errorf("undo.Save: read %s: %w", pathname, readErr)
			}
		}
		// A nil []byte binds as an empty blob under some drivers (ncruces),
		// so a missing file's content is bound as an untyped nil, NULL.
		var content any = data
		if !exists {
			content = nil
		}
		if _, err := ins.Exec(pathname, content, exists, isExec, isLink); err != nil {
			return fmt.Errorf("undo.Save: insert undo row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("undo.Save: rows iteration: %w", err)
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

// Undo restores the state saved by the most recent Save call.
// It requires undo_available == 1.
func Undo(ckout *sql.DB, dir string) error {
	if ckout == nil {
		panic("undo.Undo: ckout must not be nil")
	}
	if dir == "" {
		panic("undo.Undo: dir must not be empty")
	}
	return swapState(ckout, dir, false)
}

// Redo reverses the most recent Undo.
// It requires undo_available == 2.
func Redo(ckout *sql.DB, dir string) error {
	if ckout == nil {
		panic("undo.Redo: ckout must not be nil")
	}
	if dir == "" {
		panic("undo.Redo: dir must not be empty")
	}
	return swapState(ckout, dir, true)
}

func swapState(ckout *sql.DB, dir string, redo bool) error {
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

	if err := swapDiskFiles(tx, dir, label, wantFlag); err != nil {
		return err
	}
	if err := swapTables(tx, label); err != nil {
		return err
	}
	if err := swapCheckout(tx, label, setAvail); err != nil {
		return err
	}

	return tx.Commit()
}

// swapDiskFiles swaps file contents between disk and the undo table rows.
func swapDiskFiles(tx *sql.Tx, dir, label string, wantFlag bool) error {
	rows, err := tx.Query("SELECT rowid, pathname, content, existsflag FROM undo WHERE redoflag=?", wantFlag)
	if err != nil {
		return fmt.Errorf("undo.%s: query undo: %w", label, err)
	}

	type entry struct {
		rowid      int64
		pathname   string
		oldContent []byte
		oldExists  bool
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.rowid, &e.pathname, &e.oldContent, &e.oldExists); err != nil {
			rows.Close()
			return fmt.Errorf("undo.%s: scan undo: %w", label, err)
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("undo.%s: rows iteration: %w", label, err)
	}

	for _, e := range entries {
		fullPath := filepath.Join(dir, e.pathname)

		// Read current file to save for redo/undo.
		curData, readErr := os.ReadFile(fullPath)
		curExists := true
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				curExists = false
				curData = nil
			} else {
				return fmt.Errorf("undo.%s: read current %s: %w", label, e.pathname, readErr)
			}
		}

		// Restore old file.
		if e.oldExists {
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
				return fmt.Errorf("undo.%s: mkdir for %s: %w", label, e.pathname, err)
			}
			if err := os.WriteFile(fullPath, e.oldContent, 0o644); err != nil {
				return fmt.Errorf("undo.%s: write %s: %w", label, e.pathname, err)
			}
			if !curExists {
				fmt.Printf("NEW    %s\n", e.pathname)
			} else {
				fmt.Printf("%s %s\n", labelUpper(label), e.pathname)
			}
		} else {
			if curExists {
				if err := os.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("undo.%s: remove %s: %w", label, e.pathname, err)
				}
				fmt.Printf("DELETE %s\n", e.pathname)
			}
		}

		// Swap: update undo row with current content and flip redoflag.
		if _, err := tx.Exec("UPDATE undo SET content=?, existsflag=?, redoflag=? WHERE rowid=?",
			curData, curExists, !wantFlag, e.rowid); err != nil {
			return fmt.Errorf("undo.%s: update undo row: %w", label, err)
		}
	}
	return nil
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
