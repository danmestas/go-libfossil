package libfossil

import (
	"context"
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// CommitOpts configures a commit operation.
type CommitOpts struct {
	// Files is the caller-supplied subset of the new commit's tree. When
	// ParentID is non-zero, Files is MERGED with the parent's tracked files:
	// any name in Files overrides the parent's entry, and any file tracked
	// at the parent but not in Files is carried forward into the new manifest.
	// This matches `fossil ci`'s full-tree semantics — supplying just the
	// changed files does not erase the rest of the tree.
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
	// PartialManifest, when true, skips the parent-file merge: the resulting
	// manifest contains only Files. This is the legacy pre-fix behavior and
	// effectively erases every file tracked at the parent that is not in
	// Files. Default false matches `fossil ci`. Set true only when the
	// caller intentionally wants omission-to-mean-deletion (e.g., tests for
	// the diff/deletion path); a dedicated deletion API may replace this
	// escape hatch later.
	PartialManifest bool
	// NoSign stores the check-in unsigned even when the repository's
	// clearsign setting is on (SettingClearsign): fossil's commit --nosign.
	NoSign bool
}

// FileToCommit describes a file to include in a commit. OldName, when set,
// records that the file was renamed from that name in the parent, as
// fossil's commit records a rename (the F card's old-name field).
//
// UUID, when set, carries an artifact the repository already holds into the
// check-in by its name: the F card names it verbatim and nothing is read or
// stored, so a caller that knows a file's hash from ListFiles can carry it
// forward without its content. Content must be nil when UUID is set, and
// UUID must be a lowercase SHA1 or SHA3-256 hash. The
// artifact must be held, except under a store cone that elides it
// (draft-fossil-sparse-cone-00 §5.3); an unknown or phantom UUID fails the
// commit. This is what PartialManifest callers use to name the unchanged
// part of a tree without reading it.
type FileToCommit struct {
	Name    string
	Content []byte
	Perm    string
	OldName string
	UUID    string
}

// TagSpec describes a tag to attach to an artifact.
type TagSpec struct {
	Name  string
	Value string
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
// the RID (row ID) and UUID of the newly created artifact. When ParentID
// is non-zero, files tracked at the parent but absent from opts.Files are
// carried forward into the new manifest (full-tree semantics — see the
// CommitOpts.Files doc).
func (r *Repo) Commit(opts CommitOpts) (int64, string, error) {
	return r.commit(context.Background(), opts)
}

func (r *Repo) commit(ctx context.Context, opts CommitOpts) (int64, string, error) {
	files := make([]manifest.File, len(opts.Files))
	for i, f := range opts.Files {
		if f.UUID != "" && f.Content != nil {
			return 0, "", fmt.Errorf("libfossil: commit: file %q carries both UUID and Content", f.Name)
		}
		files[i] = manifest.File{
			Name:    f.Name,
			Content: f.Content,
			Perm:    f.Perm,
			OldName: f.OldName,
			UUID:    f.UUID,
		}
	}
	if opts.ParentID > 0 && !opts.PartialManifest {
		merged, err := manifest.MergeParentFiles(ctx, r.inner, fsltype.FslID(opts.ParentID), files)
		if err != nil {
			return 0, "", fmt.Errorf("libfossil: commit: %w", err)
		}
		files = merged
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
		NoSign:       opts.NoSign,
	}
	rid, uuid, err := manifest.CheckinContext(ctx, r.inner, copts)
	if err != nil {
		return 0, "", fmt.Errorf("libfossil: commit: %w", err)
	}
	return int64(rid), uuid, nil
}

// Ancestry walks the primary-parent chain starting from opts.Start: it
// emits that check-in, follows plink where isprim=1 to its parent, and
// repeats until the chain ends or opts.Limit is reached. This only ever
// sees first-parent ancestors of opts.Start — a merge's second parent, a
// sibling branch head, or any check-in outside that one chain will never
// appear. Use Timeline for a repository-wide view.
func (r *Repo) Ancestry(opts LogOpts) ([]LogEntry, error) {
	entries, err := manifest.Log(r.inner, manifest.LogOpts{
		Start: fsltype.FslID(opts.Start),
		Limit: opts.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("libfossil: ancestry: %w", err)
	}
	result := make([]LogEntry, len(entries))
	for i, e := range entries {
		result[i] = LogEntry{
			RID:            int64(e.RID),
			UUID:           e.UUID,
			Comment:        e.Comment,
			User:           e.User,
			Time:           e.Time,
			Kind:           e.Kind,
			Parents:        e.Parents,
			HorizonParents: e.HorizonParents,
		}
	}
	return result, nil
}

// Timeline enumerates the repository's events newest-first: every row of
// the event table by default, or just the rows matching opts.Type when
// set (canonical `fossil timeline`'s -t/--type default: no filter unless
// one is given). Unlike Ancestry, this does not start from or require any
// particular check-in — it is a repository-wide view, not a walk.
//
// Ordering is (mtime DESC, rid DESC), a total order with rid as a true
// tie-break at exact mtime equality — a deliberate improvement over
// canonical fossil, which orders by mtime DESC alone with no tie-break,
// and paginates with a bare-timestamp web cursor carrying a one-second
// slop, so rows at a page boundary there can repeat or be skipped. Do not
// "fix" this back to canonical: it is intentional. To page through the
// full result set, pass the last entry's Cursor back as the next call's
// TimelineOpts.After — never construct a cursor from a TimelineEntry's
// Time by hand; see Cursor's doc comment for why that round trip is lossy.
func (r *Repo) Timeline(opts TimelineOpts) ([]TimelineEntry, error) {
	entries, err := manifest.Timeline(r.inner, manifest.TimelineOpts{
		Type:  opts.Type,
		After: opts.After,
		Limit: opts.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("libfossil: timeline: %w", err)
	}
	result := make([]TimelineEntry, len(entries))
	for i, e := range entries {
		result[i] = TimelineEntry{
			LogEntry: LogEntry{
				RID:            int64(e.RID),
				UUID:           e.UUID,
				Comment:        e.Comment,
				User:           e.User,
				Time:           e.Time,
				Kind:           e.Kind,
				Parents:        e.Parents,
				HorizonParents: e.HorizonParents,
			},
			Cursor: e.Cursor,
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
