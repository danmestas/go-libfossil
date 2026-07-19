package manifest

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/danmestas/libfossil/db"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/repo"
)

type LogOpts struct {
	Start libfossil.FslID
	Limit int
}

type LogEntry struct {
	RID     libfossil.FslID
	UUID    string
	Comment string
	User    string
	Time    time.Time
	Parents []string
}

func Log(r *repo.Repo, opts LogOpts) ([]LogEntry, error) {
	if r == nil {
		panic("manifest.Log: r must not be nil")
	}
	if opts.Start <= 0 {
		return nil, fmt.Errorf("manifest.Log: invalid start rid %d", opts.Start)
	}
	var entries []LogEntry
	current := opts.Start
	for {
		if opts.Limit > 0 && len(entries) >= opts.Limit {
			break
		}
		// event.user and event.comment are nullable in fossil's schema: the
		// U-card and C-card are optional on a check-in manifest (the only
		// required cards are "D" and "Z"). A NULL here is valid data, not
		// malformed input, so scan through sql.NullString and let its zero
		// value ("") stand in for "no value recorded" — the same substitution
		// fossil itself makes when reading these columns.
		var uuid string
		var user, comment sql.NullString
		var mtimeScanned any
		err := r.DB().QueryRow(
			"SELECT b.uuid, e.user, e.comment, e.mtime FROM blob b JOIN event e ON e.objid=b.rid WHERE b.rid=?",
			current,
		).Scan(&uuid, &user, &comment, &mtimeScanned)
		if err != nil {
			return nil, fmt.Errorf("manifest.Log: rid=%d: %w", current, err)
		}
		// mtime is a julianday float. modernc returns float64;
		// ncruces returns time.Time for DATETIME/TIMESTAMP columns. Handle both.
		mtime, ok := db.ScanJulianDay(mtimeScanned)
		if !ok {
			return nil, fmt.Errorf("manifest.Log: rid=%d: unexpected mtime type %T", current, mtimeScanned)
		}
		var parents []string
		rows, err := r.DB().Query(
			"SELECT b.uuid FROM plink p JOIN blob b ON b.rid=p.pid WHERE p.cid=? ORDER BY p.isprim DESC",
			current,
		)
		if err == nil {
			for rows.Next() {
				var puuid string
				if err := rows.Scan(&puuid); err != nil {
					continue
				}
				parents = append(parents, puuid)
			}
			rows.Close()
		}
		entries = append(entries, LogEntry{
			RID: current, UUID: uuid, Comment: comment.String,
			User: user.String, Time: libfossil.JulianToTime(mtime), Parents: parents,
		})
		var parentRid int64
		if err := r.DB().QueryRow(
			"SELECT pid FROM plink WHERE cid=? AND isprim=1", current,
		).Scan(&parentRid); err != nil {
			break
		}
		current = libfossil.FslID(parentRid)
	}
	return entries, nil
}
