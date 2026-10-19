package checkout

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/danmestas/go-libfossil/internal/vfile"
)

// filePerm is a file's permission as fossil records it: fossil's PERM_REG,
// PERM_EXE and PERM_LNK.
type filePerm int

const (
	permReg filePerm = iota
	permExe
	permLnk
)

// recordedPerm is the permission vfile records for a row, as fossil's scan
// reads it: isexe wins over islink.
func recordedPerm(isexe, islink int64) filePerm {
	if isexe != 0 {
		return permExe
	}
	if islink != 0 {
		return permLnk
	}
	return permReg
}

// manifestPerm is the F-card permission for p.
func (p filePerm) manifestPerm() string {
	switch p {
	case permExe:
		return "x"
	case permLnk:
		return "l"
	}
	return ""
}

// linkStorage is the optional storage half that has file modes and symbolic
// links. Storages without them (memory, browser) do not have it.
type linkStorage interface {
	Lstat(path string) (os.FileInfo, error)
	Readlink(path string) (string, error)
}

// symlinker is the optional storage method that creates a symbolic link.
type symlinker interface {
	Symlink(oldname, newname string) error
}

// diskView is how a checkout sees files on disk, as fossil's RepoFILE does.
type diskView struct {
	// links is the storage's link half, nil when permissions are not read
	// from disk: a storage without modes, or Windows, where fossil keeps the
	// recorded permissions.
	links linkStorage
	// symlinks is fossil's allow-symlinks setting where links are read:
	// a symlink is then seen as itself, and otherwise as what it points to.
	symlinks bool
}

// newDiskView reads the view for the checkout's storage and repository.
func (c *Checkout) newDiskView() (diskView, error) {
	links, ok := c.env.Storage.(linkStorage)
	if !ok || runtime.GOOS == "windows" {
		return diskView{}, nil
	}
	on, err := c.boolSetting("allow-symlinks", false)
	if err != nil {
		return diskView{}, err
	}
	return diskView{links: links, symlinks: on}, nil
}

// stat is fossil's file stat for a checkout file: it does not follow a
// symlink while allow-symlinks is on. A missing file returns nil info and no
// error.
func (v diskView) stat(c *Checkout, fullPath string) (os.FileInfo, error) {
	var info os.FileInfo
	var err error
	if v.symlinks {
		info, err = v.links.Lstat(fullPath)
	} else {
		info, err = c.env.Storage.Stat(fullPath)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", fullPath, err)
	}
	return info, nil
}

// isFileOrLink is fossil's file_isfile_or_link on info from stat.
func isFileOrLink(info os.FileInfo) bool {
	return info != nil && (info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0)
}

// perm is fossil's file_perm on info from stat, or recorded where
// permissions are not read from disk. A missing file is permReg.
func (v diskView) perm(info os.FileInfo, recorded filePerm) filePerm {
	if v.links == nil {
		return recorded
	}
	if info == nil {
		return permReg
	}
	if modeIsExecutable(info.Mode()) {
		return permExe
	}
	if v.symlinks && info.Mode()&os.ModeSymlink != 0 {
		return permLnk
	}
	return permReg
}

// mtime returns the vfile.mtime to record for the file at fullPath, just
// written, statting it as fossil's vfile_to_disk does (vfile.WrittenMTime),
// a symlink seen as itself.
func (v diskView) mtime(c *Checkout, fullPath string) (int64, error) {
	now := c.env.Clock.Now().Unix()
	info, err := v.stat(c, fullPath)
	if err != nil {
		return 0, err
	}
	if info == nil {
		return 0, fmt.Errorf("stat %s: %w", fullPath, os.ErrNotExist)
	}
	return vfile.RecordableMTime(info.ModTime(), now), nil
}

// write puts data at fullPath as fossil's vfile_to_disk does. Something
// already there is deleted first when it is a symlink or a symlink is to be
// written, so a write never goes through a link. A symlink entry (link) is
// written as a symlink to data where links are seen (allow-symlinks on) and
// the storage can make them; otherwise, as in fossil, as a plain file
// holding the target's name.
func (v diskView) write(c *Checkout, fullPath string, data []byte, exe, link bool) error {
	info, err := v.stat(c, fullPath)
	if err != nil {
		return err
	}
	if info != nil && info.IsDir() {
		return fmt.Errorf("%s is a directory, cannot overwrite", fullPath)
	}
	if info != nil && (link || info.Mode()&os.ModeSymlink != 0) {
		if err := c.env.Storage.Remove(fullPath); err != nil {
			return fmt.Errorf("remove %s: %w", fullPath, err)
		}
	}
	if maker, ok := c.env.Storage.(symlinker); ok && link && v.symlinks {
		if err := maker.Symlink(string(data), fullPath); err != nil {
			return fmt.Errorf("symlink %s: %w", fullPath, err)
		}
		return nil
	}
	perm := os.FileMode(0o644)
	if exe {
		perm = 0o755
	}
	if err := c.env.Storage.WriteFile(fullPath, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", fullPath, err)
	}
	return nil
}

// read returns a checkout file's content as fossil reads it: a symlink seen
// as itself holds the name of its target.
func (v diskView) read(c *Checkout, fullPath string, info os.FileInfo) ([]byte, error) {
	if info != nil && info.Mode()&os.ModeSymlink != 0 {
		if v.links == nil {
			panic("checkout.diskView.read: a symlink seen without link support")
		}
		target, err := v.links.Readlink(fullPath)
		if err != nil {
			return nil, fmt.Errorf("readlink %s: %w", fullPath, err)
		}
		return []byte(target), nil
	}
	data, err := c.env.Storage.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fullPath, err)
	}
	return data, nil
}

// nextPermState is the permission half of fossil's vfile_check_signature:
// the chnged value its scan stores given the content half's verdict chnged
// and the file's recorded and current permission. Becoming or ceasing to be
// a symlink overrides any other change; a change of the executable bit is
// recorded only on a file not otherwise changed, and a mode change that has
// been undone returns to 0.
func nextPermState(chnged int64, orig, current filePerm) int64 {
	if chnged < 0 {
		panic("checkout.nextPermState: negative chnged")
	}
	switch {
	case orig != permLnk && current == permLnk:
		return 7
	case orig == permLnk && current != permLnk:
		return 9
	case chnged != 0 && (chnged < 6 || chnged > 9):
		return chnged
	case orig == current:
		return 0
	case current == permExe:
		return 6
	case orig == permExe:
		return 8
	}
	panic("checkout.nextPermState: permissions differ with neither a symlink nor exec")
}
