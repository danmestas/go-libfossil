//go:build js

package checkout

import "database/sql"

// updateUndo is empty under js/wasm, which has no undo package: Update
// saves no undo state there, and cannot put files back after a failure.
type updateUndo struct{}

func (c *Checkout) readUpdateUndo([]updateStep) (updateUndo, error) { return updateUndo{}, nil }

func (c *Checkout) readFilesUndo([]string) (updateUndo, error) { return updateUndo{}, nil }

func (updateUndo) save(*sql.Tx) error { return nil }

func (c *Checkout) rollBackUpdate(updateUndo) error { return nil }
