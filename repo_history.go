package libfossil

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/libfossil/internal/annotate"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/diff"
	"github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/manifest"
)

// ErrFileNotFound is returned by ReadFile when the requested filePath is
// not tracked in the given checkin. Callers can match with errors.Is.
var ErrFileNotFound = errors.New("libfossil: file not found in checkin")

// LogOpts configures a log/timeline query.
type LogOpts struct {
	Start int64
	Limit int
}

// LogEntry represents a single checkin in the timeline.
type LogEntry struct {
	RID     int64
	UUID    string
	Comment string
	User    string
	Time    time.Time
	Parents []string
}

// DiffEntry describes a unified diff for a single file.
type DiffEntry struct {
	Name    string
	Unified string
}

// AnnotatedLine is a single line of blame/annotate output.
type AnnotatedLine struct {
	Text string
	UUID string
	User string
	Date time.Time
}

// AnnotateOpts configures an annotate operation.
type AnnotateOpts struct {
	FilePath string
	StartRID int64
}

// StatusOpts configures a working-tree status query.
type StatusOpts struct {
	Dir string
}

// BisectSession holds state for a binary-search bisect operation.
type BisectSession struct {
	inner interface{}
}

// Annotate attributes each line of a file to the commit that last changed it.
func (r *Repo) Annotate(opts AnnotateOpts) ([]AnnotatedLine, error) {
	lines, err := annotate.Annotate(r.inner, annotate.Options{
		FilePath: opts.FilePath,
		StartRID: fsltype.FslID(opts.StartRID),
	})
	if err != nil {
		return nil, fmt.Errorf("libfossil: annotate: %w", err)
	}
	result := make([]AnnotatedLine, len(lines))
	for i, l := range lines {
		result[i] = AnnotatedLine{
			Text: l.Text,
			UUID: l.Version.UUID,
			User: l.Version.User,
			Date: l.Version.Date,
		}
	}
	return result, nil
}

// Diff returns a unified diff for filePath between two checkins.
// When the file is absent from a side, that side is treated as empty
// bytes, so additions and deletions render as pure insert/delete hunks.
// Returns an empty slice when both sides are byte-identical.
func (r *Repo) Diff(ridA, ridB int64, filePath string) ([]DiffEntry, error) {
	if filePath == "" {
		return nil, fmt.Errorf("libfossil: diff: filePath is required")
	}
	a, err := blobAt(r, ridA, filePath)
	if err != nil {
		return nil, fmt.Errorf("libfossil: diff: checkin %d: %w", ridA, err)
	}
	b, err := blobAt(r, ridB, filePath)
	if err != nil {
		return nil, fmt.Errorf("libfossil: diff: checkin %d: %w", ridB, err)
	}
	unified := diff.Unified(a, b, diff.Options{
		ContextLines: 3,
		SrcName:      "a/" + filePath,
		DstName:      "b/" + filePath,
	})
	if unified == "" {
		return []DiffEntry{}, nil
	}
	return []DiffEntry{{Name: filePath, Unified: unified}}, nil
}

// ReadFile returns the bytes of filePath as they existed in checkin rid.
// Returns ErrFileNotFound (wrapped) if the file is not tracked in that
// checkin. A file that exists but is empty returns ([]byte{}, nil).
func (r *Repo) ReadFile(rid int64, filePath string) ([]byte, error) {
	if filePath == "" {
		return nil, fmt.Errorf("libfossil: read file: filePath is required")
	}
	data, err := blobAt(r, rid, filePath)
	if err != nil {
		return nil, fmt.Errorf("libfossil: read file: checkin %d: %w", rid, err)
	}
	if data == nil {
		return nil, fmt.Errorf("libfossil: read file: %q in checkin %d: %w", filePath, rid, ErrFileNotFound)
	}
	return data, nil
}

// ResolveVersion resolves a symbolic version name to a repository artifact RID.
//
// Resolution order:
//  1. "" or "tip"  — newest checkin by mtime from the event table.
//  2. "trunk"      — tip of the trunk branch via tagxref/tag; falls back to "tip"
//     if the repository has no trunk tag.
//  3. Named branch — tag lookup for "sym-<name>" in tagxref/tag (e.g. "feature-x"
//     resolves via sym-feature-x).
//  4. Full UUID (≥40 chars) — exact match against blob.uuid.
//  5. UUID prefix (4–39 chars) — unique-prefix match; returns ErrAmbiguousVersion
//     if more than one artifact matches.
//
// An empty result or no match returns ErrVersionNotFound (wrapped).
// An ambiguous prefix returns ErrAmbiguousVersion (wrapped).
func (r *Repo) ResolveVersion(name string) (int64, error) {
	db := r.inner.DB()

	switch name {
	case "", "tip":
		var rid int64
		err := db.QueryRow(
			"SELECT objid FROM event WHERE type='ci' ORDER BY mtime DESC LIMIT 1",
		).Scan(&rid)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, ErrVersionNotFound)
		}
		if err != nil {
			return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, err)
		}
		return rid, nil

	case "trunk":
		var rid int64
		err := db.QueryRow(`
			SELECT tagxref.rid FROM tagxref
			JOIN tag ON tag.tagid = tagxref.tagid
			WHERE tag.tagname = 'sym-trunk'
			  AND tagxref.tagtype > 0
			ORDER BY tagxref.mtime DESC LIMIT 1`,
		).Scan(&rid)
		if err == nil {
			return rid, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, err)
		}
		// No trunk tag — fall back to tip.
		return r.ResolveVersion("tip")

	default:
		// First try as a named branch ("sym-<name>").
		var rid int64
		err := db.QueryRow(`
			SELECT tagxref.rid FROM tagxref
			JOIN tag ON tag.tagid = tagxref.tagid
			WHERE tag.tagname = ?
			  AND tagxref.tagtype > 0
			ORDER BY tagxref.mtime DESC LIMIT 1`,
			"sym-"+name,
		).Scan(&rid)
		if err == nil {
			return rid, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, err)
		}

		// Try as a UUID or UUID prefix.
		rows, err := db.Query(
			"SELECT rid FROM blob WHERE uuid LIKE ?", name+"%",
		)
		if err != nil {
			return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, err)
		}
		defer rows.Close()

		var matches []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, err)
			}
			matches = append(matches, id)
		}
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, err)
		}

		switch len(matches) {
		case 0:
			return 0, fmt.Errorf("libfossil: resolve version %q: %w", name, ErrVersionNotFound)
		case 1:
			return matches[0], nil
		default:
			return 0, fmt.Errorf("libfossil: resolve version %q matches %d artifacts: %w",
				name, len(matches), ErrAmbiguousVersion)
		}
	}
}

// ReadFileAt reads filePath from the checkin identified by a symbolic version
// name (e.g. "tip", "trunk", a branch name, a UUID, or a UUID prefix).
// It calls ResolveVersion to obtain the RID, then delegates to ReadFile.
// Use ReadFile directly when you already have a numeric RID.
func (r *Repo) ReadFileAt(version string, filePath string) ([]byte, error) {
	rid, err := r.ResolveVersion(version)
	if err != nil {
		return nil, fmt.Errorf("libfossil: read file at %q: %w", version, err)
	}
	return r.ReadFile(rid, filePath)
}

// blobAt returns the bytes of filePath as they exist in the given checkin.
// A checkin that does not contain filePath returns (nil, nil); callers treat
// that as "empty side" for diff purposes. Errors surface only for real I/O
// or consistency problems (unknown RID, missing blob for a listed UUID).
func blobAt(r *Repo, checkinRID int64, filePath string) ([]byte, error) {
	files, err := manifest.ListFiles(r.inner, fsltype.FslID(checkinRID))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.Name != filePath {
			continue
		}
		rid, ok := blob.Exists(r.DB(), f.UUID)
		if !ok {
			return nil, fmt.Errorf("blob not found for uuid %s", f.UUID)
		}
		return content.Expand(r.DB(), rid)
	}
	return nil, nil
}
