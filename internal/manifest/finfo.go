package manifest

import (
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/db"
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

	// Look up fnid for the given filename.
	var fnid int64
	err := r.DB().QueryRow("SELECT fnid FROM filename WHERE name=?", opts.Path).Scan(&fnid)
	if err != nil {
		return nil, fmt.Errorf("manifest.FileHistory: file %q not found in filename table: %w", opts.Path, err)
	}

	return fileHistoryByFnid(r, fnid, opts)
}

func fileHistoryByFnid(r *repo.Repo, fnid int64, opts FileHistoryOpts) ([]FileVersion, error) {
	limitClause := ""
	if opts.Limit > 0 {
		limitClause = fmt.Sprintf(" LIMIT %d", opts.Limit)
	}

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
		WHERE m.fnid = ?
		ORDER BY e.mtime DESC` + limitClause

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
	if pid == 0 {
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

// FileAt returns the file blob RID for a given filename at a specific checkin.
// Returns 0, false if the file does not exist in that checkin.
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

	var fid int64
	err := r.DB().QueryRow(`
		SELECT m.fid FROM mlink m
		JOIN filename f ON f.fnid = m.fnid
		WHERE m.mid = ? AND f.name = ? AND m.fid > 0`,
		checkinRID, path,
	).Scan(&fid)
	if err != nil {
		return 0, false
	}
	return libfossil.FslID(fid), true
}
