//go:build !js

package checkout

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/undo"
)

// Undo reverses the last operation that saved undo state (Update, for
// one), as fossil's undo does: the working files it saved come back, and
// the file list, the pending merges and the checked-out version are as they
// were before it. It returns the working files it changed, in the order it
// changed them, for the caller to report as fossil's undo prints them.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Undo() ([]undo.Change, error) {
	if c == nil {
		panic("checkout.Undo: nil *Checkout")
	}
	changes, err := c.swapUndo(false)
	if err != nil {
		return nil, fmt.Errorf("checkout.Undo: %w", err)
	}
	return changes, nil
}

// Redo reverses the last Undo, returning the working files it changed as
// Undo does.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Redo() ([]undo.Change, error) {
	if c == nil {
		panic("checkout.Redo: nil *Checkout")
	}
	changes, err := c.swapUndo(true)
	if err != nil {
		return nil, fmt.Errorf("checkout.Redo: %w", err)
	}
	return changes, nil
}

// swapUndo is undo.Swap on the checkout, its files seen as fossil's RepoFILE
// sees them.
func (c *Checkout) swapUndo(redo bool) ([]undo.Change, error) {
	if err := c.checkUndoCone(redo); err != nil {
		return nil, err
	}
	disk, err := c.newDiskView()
	if err != nil {
		return nil, err
	}
	return undo.Swap(c.db, disk, c.dir, redo, c.versionHash)
}

// checkUndoCone leaves older, wider snapshots intact until they can be
// restored without materializing or managing an excluded path.
func (c *Checkout) checkUndoCone(redo bool) error {
	want, op := "1", "Undo"
	if redo {
		want, op = "2", "Redo"
	}
	available, err := getVVar(c.db, "undo_available")
	if err != nil || available != want {
		return err
	}
	rows, err := c.db.Query("SELECT pathname FROM undo_vfile UNION SELECT pathname FROM undo ORDER BY pathname")
	if err != nil {
		return err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		paths = append(paths, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := c.refuseUncovered(op, paths...); err != nil {
		return err
	}
	var stamped, sparseImage bool
	if err := c.db.QueryRow("SELECT EXISTS(SELECT 1 FROM vvar WHERE name='undo_sparse_cone'), EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='undo_sparse_pending')").Scan(&stamped, &sparseImage); err != nil {
		return err
	}
	if !stamped && (sparseImage || c.cone != nil) {
		return fmt.Errorf("legacy sparse undo image has no materialization cone; refusing %s", op)
	}
	saved, err := getVVar(c.db, "undo_sparse_cone")
	if err != nil {
		return err
	}
	current, err := getVVar(c.db, coneVVar)
	if err != nil {
		return err
	}
	if saved != current {
		return fmt.Errorf("saved undo image belongs to a different sparse cone; restore its cone before %s", op)
	}
	return nil
}

// versionHash returns the hash of check-in rid.
func (c *Checkout) versionHash(rid int64) (string, error) {
	var uuid string
	if err := c.repo.DB().QueryRow("SELECT uuid FROM blob WHERE rid = ?", rid).Scan(&uuid); err != nil {
		return "", fmt.Errorf("hash of version %d: %w", rid, err)
	}
	return uuid, nil
}
