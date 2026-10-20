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
	disk, err := c.newDiskView()
	if err != nil {
		return nil, err
	}
	return undo.Swap(c.db, disk, c.dir, redo, c.versionHash)
}

// versionHash returns the hash of check-in rid.
func (c *Checkout) versionHash(rid int64) (string, error) {
	var uuid string
	if err := c.repo.DB().QueryRow("SELECT uuid FROM blob WHERE rid = ?", rid).Scan(&uuid); err != nil {
		return "", fmt.Errorf("hash of version %d: %w", rid, err)
	}
	return uuid, nil
}
