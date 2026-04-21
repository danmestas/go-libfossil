package libfossil

import (
	"fmt"
	"time"

	"github.com/danmestas/libfossil/internal/deck"
	"github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/manifest"
)

// CommitOpts configures a commit operation.
type CommitOpts struct {
	Files    []FileToCommit
	Comment  string
	User     string
	Tags     []TagSpec
	Time     time.Time
	ParentID int64
	// MergeParents lists additional parents for a merge commit. The resulting
	// manifest's P-card is [ParentID, MergeParents...]; ParentID is the primary
	// parent (branch tip being committed onto), and each MergeParent contributes
	// a secondary plink row (isprim=0).
	MergeParents []int64
	Delta        bool
}

// FileToCommit describes a file to include in a commit.
type FileToCommit struct {
	Name    string
	Content []byte
	Perm    string
}

// TagSpec describes a tag to attach to an artifact.
type TagSpec struct {
	Name  string
	Value string
}

// CheckoutOpts configures a checkout extraction.
type CheckoutOpts struct {
	Dir   string
	Force bool
}

// FileEntry describes a file in a manifest.
type FileEntry struct {
	Name string
	UUID string
	Perm string
}

// StatusEntry describes a changed file in the working tree.
type StatusEntry struct {
	Name   string
	Change string
}

// Commit creates a new checkin manifest with the given files and returns
// the RID (row ID) and UUID of the newly created artifact.
func (r *Repo) Commit(opts CommitOpts) (int64, string, error) {
	files := make([]manifest.File, len(opts.Files))
	for i, f := range opts.Files {
		files[i] = manifest.File{
			Name:    f.Name,
			Content: f.Content,
			Perm:    f.Perm,
		}
	}
	var tagCards []deck.TagCard
	for _, t := range opts.Tags {
		tagCards = append(tagCards, deck.TagCard{
			UUID:  "*",
			Type:  deck.TagPropagating,
			Name:  t.Name,
			Value: t.Value,
		})
	}
	var mergeParents []fsltype.FslID
	if len(opts.MergeParents) > 0 {
		mergeParents = make([]fsltype.FslID, len(opts.MergeParents))
		for i, mp := range opts.MergeParents {
			mergeParents[i] = fsltype.FslID(mp)
		}
	}
	copts := manifest.CheckinOpts{
		Files:        files,
		Comment:      opts.Comment,
		User:         opts.User,
		Parent:       fsltype.FslID(opts.ParentID),
		MergeParents: mergeParents,
		Delta:        opts.Delta,
		Time:         opts.Time,
		Tags:         tagCards,
	}
	rid, uuid, err := manifest.Checkin(r.inner, copts)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: commit: %w", err)
	}
	return int64(rid), uuid, nil
}

// Timeline returns checkin log entries starting from the given RID.
func (r *Repo) Timeline(opts LogOpts) ([]LogEntry, error) {
	entries, err := manifest.Log(r.inner, manifest.LogOpts{
		Start: fsltype.FslID(opts.Start),
		Limit: opts.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("libfossil: timeline: %w", err)
	}
	result := make([]LogEntry, len(entries))
	for i, e := range entries {
		result[i] = LogEntry{
			RID:     int64(e.RID),
			UUID:    e.UUID,
			Comment: e.Comment,
			User:    e.User,
			Time:    e.Time,
			Parents: e.Parents,
		}
	}
	return result, nil
}

// ListFiles returns the files in a manifest identified by blob row-id.
func (r *Repo) ListFiles(rid int64) ([]FileEntry, error) {
	entries, err := manifest.ListFiles(r.inner, fsltype.FslID(rid))
	if err != nil {
		return nil, fmt.Errorf("libfossil: list files: %w", err)
	}
	result := make([]FileEntry, len(entries))
	for i, e := range entries {
		result[i] = FileEntry{
			Name: e.Name,
			UUID: e.UUID,
			Perm: e.Perm,
		}
	}
	return result, nil
}
