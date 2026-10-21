//go:build !js

package checkout

import (
	"database/sql"
	"path/filepath"

	"github.com/danmestas/go-libfossil/internal/undo"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// updateUndo is the undo state Update saves: the files its steps change, as
// they were before, and how they were seen on disk.
type updateUndo struct {
	disk  vfile.Disk
	files []undo.File
	extra []undo.File // Failure rollback only; native Undo deliberately omits these.
}

// readUpdateUndo reads the file of every step that writes or removes one,
// under its name in the checkout, as fossil's update calls undo_save.
func (c *Checkout) readUpdateUndo(steps []updateStep) (updateUndo, error) {
	var names []string
	for _, s := range steps {
		switch s.action {
		case updateWrite, updateAddOver, updateMerge, updateRemove:
		default:
			continue
		}
		names = append(names, s.name)
	}
	u, err := c.readFilesUndo(names)
	if err != nil {
		return updateUndo{}, err
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	for _, step := range steps {
		if step.target == nil || !c.covers(step.tname) || seen[step.tname] {
			continue
		}
		if step.tname == step.name {
			info, err := u.disk.Stat(filepath.Join(c.dir, step.tname))
			if err != nil {
				return updateUndo{}, err
			}
			if info == nil || info.IsDir() || vfile.IsLink(info) {
				continue
			}
			if (info.Mode().Perm()&0o100 != 0) == step.target.exe {
				continue
			}
		}
		file, err := undo.ReadFile(u.disk, c.dir, step.tname)
		if err != nil {
			return updateUndo{}, err
		}
		u.extra = append(u.extra, file)
		seen[step.tname] = true
	}
	return u, nil
}

func (c *Checkout) readFilesUndo(names []string) (updateUndo, error) {
	disk, err := c.newDiskView()
	if err != nil {
		return updateUndo{}, err
	}
	u := updateUndo{disk: disk}
	for _, name := range names {
		f, err := undo.ReadFile(u.disk, c.dir, name)
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
	return undo.RestoreFiles(u.disk, c.dir, append(u.files, u.extra...))
}
