//go:build !js

package checkout

import (
	"database/sql"

	"github.com/danmestas/go-libfossil/internal/undo"
)

// updateUndo is the undo state Update saves: the files its steps change, as
// they were before.
type updateUndo struct {
	files []undo.File
}

// readUpdateUndo reads the file of every step that writes or removes one,
// under its name in the checkout, as fossil's update calls undo_save.
func (c *Checkout) readUpdateUndo(steps []updateStep) (updateUndo, error) {
	var u updateUndo
	for _, s := range steps {
		switch s.action {
		case updateWrite, updateAddOver, updateMerge, updateRemove:
		default:
			continue
		}
		f, err := undo.ReadFile(c.env.Storage, c.dir, s.name)
		if err != nil {
			return updateUndo{}, err
		}
		u.files = append(u.files, f)
	}
	return u, nil
}

// save records u as the undo state within tx, before vfile moves.
func (u updateUndo) save(tx *sql.Tx) error {
	return undo.SaveTx(tx, u.files)
}

// rollBackUpdate puts u's files back after an update failed partway.
func (c *Checkout) rollBackUpdate(u updateUndo) error {
	return undo.RestoreFiles(c.env, c.dir, u.files)
}
