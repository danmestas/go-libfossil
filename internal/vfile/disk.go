package vfile

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/danmestas/go-libfossil/simio"
)

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

// Disk is how fossil sees a checkout's files through its RepoFILE type: with
// allow-symlinks on, a symlink is seen as itself, a file holding the name of
// its target, and otherwise as what it points to. Every read and write of a
// checkout file that fossil makes with RepoFILE goes through a Disk, so a
// write never goes through a link.
type Disk struct {
	env *simio.Env
	// links is the storage's link half, nil when permissions are not read
	// from disk: a storage without modes, or Windows, where fossil keeps the
	// recorded permissions.
	links linkStorage
	// symlinks is fossil's allow-symlinks setting where links are read.
	symlinks bool
}

// NewDisk is the Disk for env's storage with fossil's allow-symlinks setting
// at allowSymlinks. Where the storage has no links, or on Windows, links are
// never seen.
func NewDisk(env *simio.Env, allowSymlinks bool) Disk {
	if env == nil || env.Storage == nil {
		panic("vfile.NewDisk: env and its storage must be set")
	}
	links, ok := env.Storage.(linkStorage)
	if !ok || runtime.GOOS == "windows" {
		return Disk{env: env}
	}
	return Disk{env: env, links: links, symlinks: allowSymlinks}
}

// mustBeMade panics on a zero Disk: one is made by NewDisk or OpenDisk.
func (d Disk) mustBeMade() {
	if d.env == nil {
		panic("vfile.Disk: zero Disk; make one with NewDisk or OpenDisk")
	}
}

// RowQuerier is what settings are read through: a repository database.
type RowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// OpenDisk is the Disk for env's storage with the allow-symlinks setting of
// the repository repo.
func OpenDisk(env *simio.Env, repo RowQuerier) (Disk, error) {
	on, err := BoolSetting(repo, "allow-symlinks", false)
	if err != nil {
		return Disk{}, err
	}
	return NewDisk(env, on), nil
}

// ReadsPerms reports whether d reads permissions from disk; where it does
// not, fossil keeps the recorded ones.
func (d Disk) ReadsPerms() bool {
	return d.links != nil
}

// Symlinks reports whether d sees a symlink as itself (allow-symlinks on).
func (d Disk) Symlinks() bool {
	return d.symlinks
}

// Stat is fossil's file stat for a checkout file: it does not follow a
// symlink while allow-symlinks is on. A missing file returns nil info and no
// error.
func (d Disk) Stat(fullPath string) (os.FileInfo, error) {
	d.mustBeMade()
	var info os.FileInfo
	var err error
	if d.symlinks {
		info, err = d.links.Lstat(fullPath)
	} else {
		info, err = d.env.Storage.Stat(fullPath)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", fullPath, err)
	}
	return info, nil
}

// IsLink reports whether info, from Stat, is a symlink seen as itself:
// fossil's file_islink.
func IsLink(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink != 0
}

// Read returns a checkout file's content as fossil's blob_read_from_file
// reads it with RepoFILE, given info from Stat: a symlink seen as itself
// holds the name of its target.
func (d Disk) Read(fullPath string, info os.FileInfo) ([]byte, error) {
	d.mustBeMade()
	if IsLink(info) {
		if d.links == nil {
			panic("vfile.Disk.Read: a symlink seen without link support")
		}
		target, err := d.links.Readlink(fullPath)
		if err != nil {
			return nil, fmt.Errorf("readlink %s: %w", fullPath, err)
		}
		return []byte(target), nil
	}
	data, err := d.env.Storage.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fullPath, err)
	}
	return data, nil
}

// Write puts data at fullPath as fossil's vfile_to_disk, revert, undo and
// stash do, creating its directory. Something already there is deleted
// first when it is a symlink or a symlink is to be written, so a write
// never goes through a link. A symlink (link) is written as a symlink to
// data where links are seen (allow-symlinks on) and the storage can make
// them; otherwise, as fossil's symlink_create, as a plain file holding the
// target's name. The file then gets the executable bit exe (SetExe).
func (d Disk) Write(fullPath string, data []byte, exe, link bool) error {
	d.mustBeMade()
	if err := d.env.Storage.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", fullPath, err)
	}
	info, err := d.Stat(fullPath)
	if err != nil {
		return err
	}
	if info != nil && info.IsDir() {
		return fmt.Errorf("%s is a directory, cannot overwrite", fullPath)
	}
	if info != nil && (link || IsLink(info)) {
		if err := d.env.Storage.Remove(fullPath); err != nil {
			return fmt.Errorf("remove %s: %w", fullPath, err)
		}
	}
	if maker, ok := d.env.Storage.(symlinker); ok && link && d.symlinks {
		if err := maker.Symlink(string(data), fullPath); err != nil {
			return fmt.Errorf("symlink %s: %w", fullPath, err)
		}
		return nil
	}
	perm := os.FileMode(0o644)
	if exe {
		perm = 0o755
	}
	if data == nil {
		data = []byte{} // an empty stored blob scans as nil
	}
	if err := d.env.Storage.WriteFile(fullPath, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", fullPath, err)
	}
	// WriteFile keeps an existing file's mode.
	return d.SetExe(fullPath, exe)
}

// modeSetter is the optional storage method that changes a file's mode.
// Storages without file modes (memory, browser) do not have it.
type modeSetter interface {
	Chmod(path string, mode os.FileMode) error
}

// SetExe gives the file at fullPath the executable bit exe, where the
// storage has file modes, as fossil's file_setexe does: execute wherever
// read is allowed, or nowhere. A symlink seen as itself, a directory or
// nothing at all is left alone.
func (d Disk) SetExe(fullPath string, exe bool) error {
	d.mustBeMade()
	setter, ok := d.env.Storage.(modeSetter)
	if !ok {
		return nil
	}
	info, err := d.Stat(fullPath)
	if err != nil {
		return err
	}
	if info == nil || info.IsDir() || IsLink(info) {
		return nil
	}
	perm := info.Mode().Perm()
	if (perm&0o100 != 0) == exe {
		return nil
	}
	mode := perm &^ 0o111
	if exe {
		mode |= (perm & 0o444) >> 2
	}
	if err := setter.Chmod(fullPath, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", fullPath, err)
	}
	return nil
}

// MTime returns the vfile.mtime to record for the file at fullPath, just
// written, statting it as fossil's vfile_to_disk does (WrittenMTime), a
// symlink seen as itself.
func (d Disk) MTime(fullPath string) (int64, error) {
	d.mustBeMade()
	now := d.env.Clock.Now().Unix()
	info, err := d.Stat(fullPath)
	if err != nil {
		return 0, err
	}
	if info == nil {
		return 0, fmt.Errorf("stat %s: %w", fullPath, os.ErrNotExist)
	}
	return RecordableMTime(info.ModTime(), now), nil
}

// BoolSetting reads a boolean fossil setting, dflt when unset, as
// db_get_boolean reads it from the repository's config table. Fossil's
// db_get falls back to global_config in ~/.fossil (`fossil settings
// mtime-changes off --global`); go-libfossil reads no global fossil settings,
// so a global value is not seen here.
func BoolSetting(repo RowQuerier, name string, dflt bool) (bool, error) {
	if repo == nil {
		panic("vfile.BoolSetting: nil repo")
	}
	var value sql.NullString
	err := repo.QueryRow("SELECT value FROM config WHERE name = ?", name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !value.Valid) {
		return dflt, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	return settingBoolean(value.String, dflt), nil
}

// settingBoolean is fossil's db_get_boolean on a stored value: is_truth
// ("on", "yes", "true" in any case, or a nonzero C atoi) gives true, is_false
// ("off", "no", "false", "0" in any case) gives false, anything else dflt.
func settingBoolean(value string, dflt bool) bool {
	switch strings.ToLower(value) {
	case "on", "yes", "true":
		return true
	case "off", "no", "false", "0":
		return false
	}
	digits := strings.TrimLeft(value, " \t\n\v\f\r")
	if digits != "" && (digits[0] == '+' || digits[0] == '-') {
		digits = digits[1:]
	}
	digits = strings.TrimLeft(digits, "0")
	if digits != "" && digits[0] >= '1' && digits[0] <= '9' {
		return true // a nonzero atoi
	}
	return dflt
}

// Remove deletes the file at fullPath, as fossil's file_delete does: a
// symlink itself, never what it points to. Nothing there is not an error.
func (d Disk) Remove(fullPath string) error {
	d.mustBeMade()
	err := d.env.Storage.Remove(fullPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", fullPath, err)
	}
	return nil
}
