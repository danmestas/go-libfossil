package libfossil

import (
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/tag"
	"github.com/danmestas/go-libfossil/internal/uv"
)

// StashEntry describes a saved stash.
type StashEntry struct {
	ID      int64
	Comment string
	Time    string
}

// TagOpts configures a tag operation.
type TagOpts struct {
	Name     string
	TargetID int64
	Value    string
	User     string
	Time     time.Time
}

// UVEntry describes an unversioned file.
type UVEntry struct {
	Name  string
	Size  int64
	Mtime time.Time
	Hash  string
}

// Tag creates a control artifact that adds a tag to a target checkin.
// Returns the UUID of the tag control artifact.
func (r *Repo) Tag(opts TagOpts) (int64, error) {
	rid, err := tag.AddTag(r.inner, tag.TagOpts{
		TargetRID: fsltype.FslID(opts.TargetID),
		TagName:   opts.Name,
		TagType:   tag.TagSingleton,
		Value:     opts.Value,
		User:      opts.User,
		Time:      opts.Time,
	})
	if err != nil {
		return 0, fmt.Errorf("libfossil: tag: %w", err)
	}
	return int64(rid), nil
}

// StashSave saves working-tree changes to the stash.
// Requires a checkout database; not yet wired (Repo only wraps the repo DB).
func (r *Repo) StashSave(dir, comment string) error {
	return fmt.Errorf("libfossil: stash save: not yet implemented (requires checkout DB)")
}

// StashPop restores the most recent stash entry and removes it.
func (r *Repo) StashPop(dir string) error {
	return fmt.Errorf("libfossil: stash pop: not yet implemented (requires checkout DB)")
}

// StashApply restores a stash entry by ID without removing it.
func (r *Repo) StashApply(dir string, id int64) error {
	return fmt.Errorf("libfossil: stash apply: not yet implemented (requires checkout DB)")
}

// StashList returns all stash entries.
func (r *Repo) StashList() ([]StashEntry, error) {
	return nil, fmt.Errorf("libfossil: stash list: not yet implemented (requires checkout DB)")
}

// StashDrop removes a stash entry by ID.
func (r *Repo) StashDrop(id int64) error {
	return fmt.Errorf("libfossil: stash drop: not yet implemented (requires checkout DB)")
}

// StashClear removes all stash entries.
func (r *Repo) StashClear() error {
	return fmt.Errorf("libfossil: stash clear: not yet implemented (requires checkout DB)")
}

// Undo reverts the last commit or merge.
// Requires a checkout database; not yet wired.
func (r *Repo) Undo(dir string) error {
	return fmt.Errorf("libfossil: undo: not yet implemented (requires checkout DB)")
}

// Redo re-applies the last undone operation.
// Requires a checkout database; not yet wired.
func (r *Repo) Redo(dir string) error {
	return fmt.Errorf("libfossil: redo: not yet implemented (requires checkout DB)")
}

// UVWrite writes an unversioned file to the repository.
func (r *Repo) UVWrite(name string, content []byte, mtime time.Time) error {
	if err := uv.EnsureSchema(r.inner.DB()); err != nil {
		return fmt.Errorf("libfossil: uv write: %w", err)
	}
	mtimeUnix := mtime.Unix()
	if mtime.IsZero() {
		mtimeUnix = time.Now().Unix()
	}
	return uv.Write(r.inner.DB(), name, content, mtimeUnix)
}

// UVDelete marks an unversioned file as deleted (tombstone).
func (r *Repo) UVDelete(name string, mtime time.Time) error {
	if err := uv.EnsureSchema(r.inner.DB()); err != nil {
		return fmt.Errorf("libfossil: uv delete: %w", err)
	}
	mtimeUnix := mtime.Unix()
	if mtime.IsZero() {
		mtimeUnix = time.Now().Unix()
	}
	return uv.Delete(r.inner.DB(), name, mtimeUnix)
}

// UVRead reads an unversioned file from the repository.
// Returns the content, mtime (unix seconds), and content hash.
func (r *Repo) UVRead(name string) ([]byte, int64, string, error) {
	return uv.Read(r.inner.DB(), name)
}

// UVList returns all unversioned file entries.
func (r *Repo) UVList() ([]UVEntry, error) {
	if err := uv.EnsureSchema(r.inner.DB()); err != nil {
		return nil, fmt.Errorf("libfossil: uv list: %w", err)
	}
	entries, err := uv.List(r.inner.DB())
	if err != nil {
		return nil, fmt.Errorf("libfossil: uv list: %w", err)
	}
	result := make([]UVEntry, len(entries))
	for i, e := range entries {
		result[i] = UVEntry{
			Name:  e.Name,
			Size:  int64(e.Size),
			Mtime: time.Unix(e.MTime, 0),
			Hash:  e.Hash,
		}
	}
	return result, nil
}
