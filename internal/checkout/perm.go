package checkout

import (
	"os"

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

// newDiskView reads how the checkout sees files on disk, as fossil's
// RepoFILE does, from its storage and the repository's allow-symlinks.
func (c *Checkout) newDiskView() (vfile.Disk, error) {
	return vfile.OpenDisk(c.env, c.repo.DB())
}

// isFileOrLink is fossil's file_isfile_or_link on info from Disk.Stat.
func isFileOrLink(info os.FileInfo) bool {
	return info != nil && (info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0)
}

// diskPerm is fossil's file_perm on info from disk.Stat, or recorded where
// disk does not read permissions. A missing file is permReg.
func diskPerm(disk vfile.Disk, info os.FileInfo, recorded filePerm) filePerm {
	if !disk.ReadsPerms() {
		return recorded
	}
	if info == nil {
		return permReg
	}
	if modeIsExecutable(info.Mode()) {
		return permExe
	}
	if disk.Symlinks() && vfile.IsLink(info) {
		return permLnk
	}
	return permReg
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
