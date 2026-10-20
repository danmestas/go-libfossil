//go:build !js

package checkout

import (
	"database/sql"

	"github.com/danmestas/go-libfossil/internal/undo"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// revertUndo is the undo state Revert saves: the files its steps write or
// delete, as they were before, and how they were seen on disk.
type revertUndo struct {
	disk  vfile.Disk
	files []undo.File
}

// readRevertUndo reads the file under every name steps write or delete, as
// fossil's revert calls undo_save before it touches one.
func (c *Checkout) readRevertUndo(steps []revertStep) (revertUndo, error) {
	disk, err := c.newDiskView()
	if err != nil {
		return revertUndo{}, err
	}
	u := revertUndo{disk: disk}
	for _, name := range revertNames(steps) {
		f, err := undo.ReadFile(u.disk, c.dir, name)
		if err != nil {
			return revertUndo{}, err
		}
		u.files = append(u.files, f)
	}
	return u, nil
}

// save records u as the undo state within tx, before vfile changes.
func (u revertUndo) save(tx *sql.Tx) error {
	return undo.SaveTx(tx, u.files)
}

// rollBackRevert puts u's files back after a revert failed partway.
func (c *Checkout) rollBackRevert(u revertUndo) error {
	return undo.RestoreFiles(u.disk, c.dir, u.files)
}
