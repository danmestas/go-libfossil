//go:build !js

package checkout

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/undo"
)

// Undo reverses the last operation that saved undo state (Update, for
// one), as fossil's undo does: the working files it saved come back, and
// the file list, the pending merges and the checked-out version are as they
// were before it.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Undo() error {
	if c == nil {
		panic("checkout.Undo: nil *Checkout")
	}
	if err := undo.Swap(c.db, c.env, c.dir, false, c.versionHash); err != nil {
		return fmt.Errorf("checkout.Undo: %w", err)
	}
	return nil
}

// Redo reverses the last Undo.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Redo() error {
	if c == nil {
		panic("checkout.Redo: nil *Checkout")
	}
	if err := undo.Swap(c.db, c.env, c.dir, true, c.versionHash); err != nil {
		return fmt.Errorf("checkout.Redo: %w", err)
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
