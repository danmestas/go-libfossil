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
	Kind    libfossil.EventKind
	Parents []string
	// Cursor is a Timeline pagination token for this entry; zero-value
	// (invalid) for entries produced by Log/Ancestry, which paginates by
	// Start/Limit instead and has no use for it.
	Cursor libfossil.Cursor
}

// maxAncestryDepth bounds the first-parent walk in Log so that a cyclic or
// otherwise corrupt plink table cannot hang the process (TigerStyle: every
// loop needs an explicit, defensible bound). It is a var rather than a
// const so tests can shrink it to exercise the bound deterministically
// without walking a million rows. No real fossil repository's primary-parent
// chain approaches this depth.
var maxAncestryDepth = 1_000_000

func Log(r *repo.Repo, opts LogOpts) ([]LogEntry, error) {
	if r == nil {
		panic("manifest.Log: r must not be nil")
	}
	if opts.Start <= 0 {
		return nil, fmt.Errorf("manifest.Log: invalid start rid %d", opts.Start)
	}
	var entries []LogEntry
	current := opts.Start
	for depth := 0; ; depth++ {
		if depth >= maxAncestryDepth {
			return nil, fmt.Errorf("manifest.Log: exceeded max ancestry depth %d starting at rid=%d (possible plink cycle)", maxAncestryDepth, opts.Start)
		}
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
		entries = append(entries, LogEntry{
			RID: current, UUID: uuid, Comment: comment.String,
			User: user.String, Time: libfossil.JulianToTime(mtime),
			Kind: libfossil.EventKindCheckin, Parents: parentUUIDs(r, current),
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

// parentUUIDs returns the UUIDs of cid's parents (primary parent first),
// via the plink table. Best-effort: a query or scan failure yields a nil
// (empty) result rather than aborting the caller's walk or enumeration —
// matching the original manifest.Log behavior this was extracted from.
// Shared by Log (ancestry walk) and Timeline (enumeration) so entry
// construction is not duplicated between the two query paths.
func parentUUIDs(r *repo.Repo, cid libfossil.FslID) []string {
	var parents []string
	rows, err := r.DB().Query(
		"SELECT b.uuid FROM plink p JOIN blob b ON b.rid=p.pid WHERE p.cid=? ORDER BY p.isprim DESC",
		cid,
	)
	if err != nil {
		return parents
	}
	defer rows.Close()
	for rows.Next() {
		var puuid string
		if err := rows.Scan(&puuid); err != nil {
			continue
		}
		parents = append(parents, puuid)
	}
	return parents
}
