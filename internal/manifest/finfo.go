package manifest

import (
	"cmp"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// FileAction describes what happened to a file in a given checkin.
type FileAction int

const (
	FileAdded    FileAction = iota // file first appears (no parent file blob)
	FileModified                   // file content changed from parent
	FileDeleted                    // file removed (fid = 0)
	FileRenamed                    // filename changed (pfnid != fnid)
)

func (a FileAction) String() string {
	switch a {
	case FileAdded:
		return "added"
	case FileModified:
		return "modified"
	case FileDeleted:
		return "deleted"
	case FileRenamed:
		return "renamed"
	default:
		return "unknown"
	}
}

// FileVersion represents one entry in a file's history across checkins.
type FileVersion struct {
	CheckinRID  libfossil.FslID // rid of the checkin manifest
	CheckinUUID string          // UUID of the checkin
	FileRID     libfossil.FslID // rid of the file blob (0 if deleted)
	FileUUID    string          // UUID of the file blob ("" if deleted)
	Action      FileAction
	User        string
	Comment     string
	Date        time.Time
	PrevName    string // non-empty if renamed (previous filename)
}

// FileHistoryOpts controls the file history query.
type FileHistoryOpts struct {
	// Path is the filename to trace (required).
	Path string
	// Limit caps the number of results (0 = unlimited).
	Limit int
	// FoldCase matches Path to file names with ASCII letters case-folded, as
	// fossil finfo does when its case-sensitive setting is off. The history
	// of every name that matches is merged, newest first.
	FoldCase bool
}

// FileHistory returns the change history of a file across checkins,
// ordered by date descending (most recent first). It queries the mlink
// table directly, which is populated during crosslink/rebuild.
//
// Each returned FileVersion represents a checkin where the file was
// added, modified, deleted, or renamed.
func FileHistory(r *repo.Repo, opts FileHistoryOpts) ([]FileVersion, error) {
	if r == nil {
		panic("manifest.FileHistory: r must not be nil")
	}
	if opts.Path == "" {
		return nil, fmt.Errorf("manifest.FileHistory: Path is required")
	}

	collation := ""
	if opts.FoldCase {
		collation = " COLLATE nocase" // SQLite's nocase folds ASCII only, as fossil's does
	}
	rows, err := r.DB().Query("SELECT fnid FROM filename WHERE name=?"+collation, opts.Path)
	if err != nil {
		return nil, fmt.Errorf("manifest.FileHistory: file %q: %w", opts.Path, err)
	}
	var fnids []int64
	for rows.Next() {
		var fnid int64
		if err := rows.Scan(&fnid); err != nil {
			rows.Close()
			return nil, fmt.Errorf("manifest.FileHistory: file %q: %w", opts.Path, err)
		}
		fnids = append(fnids, fnid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("manifest.FileHistory: file %q: %w", opts.Path, err)
	}
	if len(fnids) == 0 {
		return nil, fmt.Errorf("manifest.FileHistory: file %q not found in filename table: %w", opts.Path, sql.ErrNoRows)
	}

	var all []FileVersion
	for _, fnid := range fnids {
		versions, err := fileHistoryByFnid(r, fnid, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, versions...)
	}
	if len(fnids) > 1 {
		// Several names differ only in case: merge their histories as one
		// query over them would, newest first, then apply the limit.
		slices.SortStableFunc(all, func(a, b FileVersion) int {
			if c := b.Date.Compare(a.Date); c != 0 {
				return c
			}
			return cmp.Compare(b.CheckinRID, a.CheckinRID)
		})
		if opts.Limit > 0 && len(all) > opts.Limit {
			all = all[:opts.Limit]
		}
	}
	return all, nil
}

func fileHistoryByFnid(r *repo.Repo, fnid int64, opts FileHistoryOpts) ([]FileVersion, error) {
	query := `
		SELECT
			m.mid,
			cb.uuid,
			m.fid,
			COALESCE(fb.uuid, ''),
			COALESCE(m.pid, 0),
			COALESCE(m.pfnid, 0),
			e.user,
			e.comment,
			e.mtime
		FROM mlink m
		JOIN blob cb ON cb.rid = m.mid
		JOIN event e ON e.objid = m.mid
		LEFT JOIN blob fb ON fb.rid = m.fid
		WHERE m.fnid = ? AND NOT m.isaux
		ORDER BY e.mtime DESC, m.mid DESC`

	rows, err := r.DB().Query(query, fnid)
	if err != nil {
		return nil, fmt.Errorf("manifest.FileHistory: query: %w", err)
	}
	defer rows.Close()

	var versions []FileVersion
	for rows.Next() {
		var v FileVersion
		var pid, pfnid int64
		var mtimeRaw any
		if err := rows.Scan(
			&v.CheckinRID, &v.CheckinUUID,
			&v.FileRID, &v.FileUUID,
			&pid, &pfnid,
			&v.User, &v.Comment, &mtimeRaw,
		); err != nil {
			return nil, fmt.Errorf("manifest.FileHistory: scan: %w", err)
		}

		v.Date = parseMtime(mtimeRaw)
		action, changed := classifyAction(v.FileRID, pid, pfnid, fnid)
		if !changed {
			continue // skip unchanged files
		}
		v.Action = action

		if pfnid != 0 && pfnid != fnid {
			var prevName string
			if err := r.DB().QueryRow("SELECT name FROM filename WHERE fnid=?", pfnid).Scan(&prevName); err == nil {
				v.PrevName = prevName
			}
		}

		versions = append(versions, v)
		// The limit counts real changes, so it is applied here and not in
		// SQL, where rows that merely list the file unchanged would use it up.
		if opts.Limit > 0 && len(versions) == opts.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("manifest.FileHistory: rows: %w", err)
	}

	return versions, nil
}

// classifyAction returns the action and whether this entry represents a real change.
// unchanged returns false when fid == pid (file listed in commit but not actually modified).
func classifyAction(fileRID libfossil.FslID, pid, pfnid, fnid int64) (FileAction, bool) {
	if fileRID == 0 {
		return FileDeleted, true
	}
	if pfnid != 0 && pfnid != fnid {
		return FileRenamed, true
	}
	if pid <= 0 {
		return FileAdded, true
	}
	if int64(fileRID) == pid {
		return FileModified, false // unchanged — same blob as parent
	}
	return FileModified, true
}

func parseMtime(raw any) time.Time {
	t, _ := db.ScanTime(raw)
	return t
}

// FileAt returns the available file blob RID for a filename resolved from a
// check-in's effective manifest tree.
// Returns 0, false if the file does not exist, is deleted, or is unavailable.
// This is a convenience wrapper for diff-between-versions use cases.
func FileAt(r *repo.Repo, checkinRID libfossil.FslID, path string) (libfossil.FslID, bool) {
	if r == nil {
		panic("manifest.FileAt: r must not be nil")
	}
	if checkinRID <= 0 {
		panic("manifest.FileAt: checkinRID must be positive")
	}
	if path == "" {
		panic("manifest.FileAt: path must not be empty")
	}

	q := r.DB()
	file, found, err := LookupFile(r, checkinRID, path)
	if err != nil || !found {
		return 0, false
	}
	return content.AvailableByUUID(q, file.UUID)
}
