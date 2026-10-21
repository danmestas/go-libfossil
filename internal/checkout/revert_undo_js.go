//go:build js

package checkout

import "database/sql"

// revertUndo is empty under js/wasm, which has no undo package: Revert
// saves no undo state there, and cannot put files back after a failure.
type revertUndo struct{}

func (c *Checkout) readRevertUndo([]revertStep) (revertUndo, error) { return revertUndo{}, nil }

func (revertUndo) save(*sql.Tx) error { return nil }

func (c *Checkout) rollBackRevert(revertUndo) error { return nil }
