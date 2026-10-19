//go:build !js

package libfossil

import "fmt"

// Undo reverses the last operation that saved undo state, such as Update,
// as fossil's undo does: the files it changed come back, and the checkout's
// file list and version are as they were before it. Fossil's undo reads
// the same state, so either tool can undo the other's update.
func (c *Checkout) Undo() error {
	if err := c.inner.Undo(); err != nil {
		return fmt.Errorf("libfossil: undo: %w", err)
	}
	return nil
}

// Redo reverses the last Undo.
func (c *Checkout) Redo() error {
	if err := c.inner.Redo(); err != nil {
		return fmt.Errorf("libfossil: redo: %w", err)
	}
	return nil
}
