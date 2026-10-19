//go:build !js

package libfossil

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/undo"
)

// UndoResult reports the working files Undo or Redo changed, each list in
// the order they were changed, fossil's order. Fossil's undo prints them as
// UNDO (or REDO), NEW and DELETE lines; a caller shows them as it likes.
type UndoResult struct {
	FilesRestored []string // existing files given back their saved content
	FilesCreated  []string // files written that were not on disk
	FilesRemoved  []string // files deleted from the working tree
}

// Undo reverses the last operation that saved undo state, such as Update,
// as fossil's undo does: the files it changed come back, and the checkout's
// file list and version are as they were before it. Fossil's undo reads
// the same state, so either tool can undo the other's update.
func (c *Checkout) Undo() (UndoResult, error) {
	changes, err := c.inner.Undo()
	if err != nil {
		return UndoResult{}, fmt.Errorf("libfossil: undo: %w", err)
	}
	return undoResult(changes), nil
}

// Redo reverses the last Undo, reporting the files it changed as Undo does.
func (c *Checkout) Redo() (UndoResult, error) {
	changes, err := c.inner.Redo()
	if err != nil {
		return UndoResult{}, fmt.Errorf("libfossil: redo: %w", err)
	}
	return undoResult(changes), nil
}

func undoResult(changes []undo.Change) UndoResult {
	var r UndoResult
	for _, ch := range changes {
		switch ch.Action {
		case undo.Restored:
			r.FilesRestored = append(r.FilesRestored, ch.Pathname)
		case undo.Created:
			r.FilesCreated = append(r.FilesCreated, ch.Pathname)
		case undo.Deleted:
			r.FilesRemoved = append(r.FilesRemoved, ch.Pathname)
		default:
			panic(fmt.Sprintf("libfossil: unknown undo action %v", ch.Action))
		}
	}
	return r
}
