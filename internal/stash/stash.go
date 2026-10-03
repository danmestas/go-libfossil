//go:build !js

// Package stash saves and restores working directory changes, storing deltas
// against baseline blobs in the checkout database (.fslckout).
package stash

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/vfile"
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

// Save stashes all changed files in the checkout, then reverts the working directory.
func Save(ckout *sql.DB, repoDB *sql.DB, dir string, comment string) error {
	if ckout == nil {
		panic("stash.Save: ckout must not be nil")
	}
	if repoDB == nil {
		panic("stash.Save: repoDB must not be nil")
	}
	if dir == "" {
		panic("stash.Save: dir must not be empty")
	}
	if err := EnsureTables(ckout); err != nil {
		return err
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

	// Insert stash header.
	if _, err := tx.Exec("INSERT INTO stash(stashid, hash, comment) VALUES(?,?,?)",
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

	if err := storeAndRevertFiles(tx, repoDB, dir, stashID, files); err != nil {
		return err
	}

	return tx.Commit()
}

// snapshotChangedFiles returns the checked-out version's changed rows,
// as vfile.Classify judges them. A pending merge or a rename is refused:
// Save records and restores files by current name against their committed
// version, which would drop the merge or lose the rename.
func snapshotChangedFiles(tx *sql.Tx) ([]vfile.Row, error) {
	if tx == nil {
		panic("stash.snapshotChangedFiles: nil tx")
	}

	var vidText string
	if err := tx.QueryRow("SELECT value FROM vvar WHERE name='checkout'").Scan(&vidText); err != nil {
		return nil, fmt.Errorf("stash.Save: read checkout version: %w", err)
	}
	vid, err := strconv.ParseInt(vidText, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("stash.Save: checkout version %q: %w", vidText, err)
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
		if r.PendingMerge() {
			return nil, fmt.Errorf(
				"stash.Save: %s has a pending merge; commit or revert it first", r.Pathname)
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
// it to its committed version.
func storeAndRevertFiles(
	tx *sql.Tx, repoDB *sql.DB, dir string, stashID int64, files []vfile.Row,
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
		baselineHash, deltaBytes, err := stashRecord(repoDB, fullPath, f)
		if err != nil {
			return err
		}
		if _, err := ins.Exec(stashID, f.IsAdded(), f.IsRemoved(), f.IsExe != 0, f.IsLink != 0,
			nullStr(baselineHash), f.Pathname, f.Pathname, deltaBytes); err != nil {
			return fmt.Errorf("stash.Save: insert stashfile %s: %w", f.Pathname, err)
		}
		if err := revertStashedFile(tx, repoDB, fullPath, f); err != nil {
			return err
		}
	}
	return nil
}

// stashRecord returns what the stash keeps for f: an added file's content
// with no baseline, a removed file's baseline hash with an empty delta, or a
// modified file's baseline hash with a delta from baseline to its content.
func stashRecord(
	repoDB *sql.DB, fullPath string, f vfile.Row,
) (baselineHash string, deltaBytes []byte, err error) {
	if f.IsAdded() {
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return "", nil, fmt.Errorf("stash.Save: read added %s: %w", f.Pathname, err)
		}
		return "", data, nil
	}

	if err := repoDB.QueryRow(
		"SELECT uuid FROM blob WHERE rid=?", f.RID,
	).Scan(&baselineHash); err != nil {
		return "", nil, fmt.Errorf("stash.Save: get uuid for rid=%d: %w", f.RID, err)
	}
	if f.IsRemoved() {
		return baselineHash, []byte{}, nil
	}
	baseline, err := content.Expand(repoDB, libfossil.FslID(f.RID))
	if err != nil {
		return "", nil, fmt.Errorf("stash.Save: expand rid=%d: %w", f.RID, err)
	}
	working, err := os.ReadFile(fullPath)
	if err != nil {
		return "", nil, fmt.Errorf("stash.Save: read %s: %w", f.Pathname, err)
	}
	return baselineHash, delta.Create(baseline, working), nil
}

// revertStashedFile undoes f's change: an added file is removed from disk
// and vfile; a removed or modified file gets its committed content back and
// its row is marked unchanged.
func revertStashedFile(tx *sql.Tx, repoDB *sql.DB, fullPath string, f vfile.Row) error {
	if f.IsAdded() {
		if err := os.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("stash.Save: mkdir for %s: %w", f.Pathname, err)
	}
	if err := os.WriteFile(fullPath, baseline, 0o644); err != nil {
		return fmt.Errorf("stash.Save: write %s: %w", f.Pathname, err)
	}
	if _, err := tx.Exec(
		"UPDATE vfile SET deleted=0, chnged=0 WHERE id=?", f.ID,
	); err != nil {
		return fmt.Errorf("stash.Save: update vfile %s: %w", f.Pathname, err)
	}
	return nil
}

// Apply restores stashed files to the working directory without removing the stash entry.
func Apply(ckout *sql.DB, repoDB *sql.DB, dir string, stashID int64) error {
	if ckout == nil {
		panic("stash.Apply: ckout must not be nil")
	}
	if repoDB == nil {
		panic("stash.Apply: repoDB must not be nil")
	}
	if dir == "" {
		panic("stash.Apply: dir must not be empty")
	}
	if stashID <= 0 {
		panic("stash.Apply: stashID must be positive")
	}
	rows, err := ckout.Query(`SELECT isAdded, isRemoved, hash, newname, delta
		FROM stashfile WHERE stashid=?`, stashID)
	if err != nil {
		return fmt.Errorf("stash.Apply: query stashfile: %w", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		found = true
		var isAdded, isRemoved bool
		var hashStr sql.NullString
		var newname string
		var deltaBytes []byte

		if err := rows.Scan(&isAdded, &isRemoved, &hashStr, &newname, &deltaBytes); err != nil {
			return fmt.Errorf("stash.Apply: scan stashfile: %w", err)
		}

		fullPath := filepath.Join(dir, newname)

		if isAdded {
			// Write raw content.
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
				return fmt.Errorf("stash.Apply: mkdir for %s: %w", newname, err)
			}
			if err := os.WriteFile(fullPath, deltaBytes, 0o644); err != nil {
				return fmt.Errorf("stash.Apply: write %s: %w", newname, err)
			}
		} else if isRemoved {
			// Delete the file.
			if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("stash.Apply: remove %s: %w", newname, err)
			}
		} else {
			// Modified: apply delta against baseline.
			if !hashStr.Valid {
				return fmt.Errorf("stash.Apply: missing baseline hash for %s", newname)
			}
			rid, ok := content.AvailableByUUID(repoDB, hashStr.String)
			if !ok {
				return fmt.Errorf("stash.Apply: baseline blob %s not found", hashStr.String)
			}
			baseline, err := content.Expand(repoDB, rid)
			if err != nil {
				return fmt.Errorf("stash.Apply: expand baseline %s: %w", hashStr.String, err)
			}
			result, err := delta.Apply(baseline, deltaBytes)
			if err != nil {
				return fmt.Errorf("stash.Apply: apply delta for %s: %w", newname, err)
			}
			if err := os.WriteFile(fullPath, result, 0o644); err != nil {
				return fmt.Errorf("stash.Apply: write %s: %w", newname, err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("stash.Apply: rows iteration: %w", err)
	}
	if !found {
		return fmt.Errorf("stash.Apply: stash %d not found", stashID)
	}
	return nil
}

// Pop applies the most recent stash entry and removes it.
func Pop(ckout *sql.DB, repoDB *sql.DB, dir string) error {
	if ckout == nil {
		panic("stash.Pop: ckout must not be nil")
	}
	if repoDB == nil {
		panic("stash.Pop: repoDB must not be nil")
	}
	if dir == "" {
		panic("stash.Pop: dir must not be empty")
	}
	var stashID int64
	err := ckout.QueryRow("SELECT stashid FROM stash ORDER BY stashid DESC LIMIT 1").Scan(&stashID)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("stash.Pop: no stash entries")
		}
		return fmt.Errorf("stash.Pop: query top stash: %w", err)
	}

	if err := Apply(ckout, repoDB, dir, stashID); err != nil {
		return err
	}
	return Drop(ckout, stashID)
}

// List returns all stash entries ordered by ID descending (most recent first).
func List(ckout *sql.DB) ([]Entry, error) {
	if ckout == nil {
		panic("stash.List: ckout must not be nil")
	}
	if err := EnsureTables(ckout); err != nil {
		return nil, err
	}

	rows, err := ckout.Query("SELECT stashid, hash, comment, ctime FROM stash ORDER BY stashid DESC")
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
