package manifest

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/danmestas/libfossil/db"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/repo"
)

// TimelineOpts configures a repository-wide event enumeration.
type TimelineOpts struct {
	// Type restricts the enumeration to a single event.type code. The zero
	// value means "all kinds" — the canonical `fossil timeline` default.
	Type libfossil.EventKind
	// After, when valid, restricts results to events strictly after this
	// cursor in Timeline's (mtime DESC, rid DESC) order — i.e. the next
	// page following whatever TimelineEntry produced it. The zero Cursor
	// means "start from the newest event". Obtain a Cursor from a
	// TimelineEntry's Cursor field; do not construct one by hand (see
	// fsltype.Cursor).
	After libfossil.Cursor
	// Limit caps the number of rows returned. Zero or negative means
	// unbounded (matches LogOpts.Limit's convention).
	Limit int
}

// TimelineEntry is a single event as returned by Timeline. It extends
// LogEntry with Cursor, the pagination token for this row: Timeline is the
// operation with pagination semantics, so its own result type — not the
// shared LogEntry — is where the cursor belongs. Cursor is always valid
// (fsltype.Cursor.Valid() == true) on every entry Timeline produces.
type TimelineEntry struct {
	LogEntry
	Cursor libfossil.Cursor
}

// maxTimelineRows bounds the enumeration's row loop so that an unbounded
// query (Limit <= 0) against a corrupt or adversarial event table cannot
// hang the process (TigerStyle: every loop needs an explicit bound). A var,
// not a const, so tests can shrink it. No real fossil repository has
// anywhere near this many events.
var maxTimelineRows = 10_000_000

// Timeline enumerates the event table newest-first: every event by
// default, or just the events matching opts.Type when set. This is the
// repository-wide enumeration fossil's own `timeline` command performs —
// it does not follow plink at all, so it is not limited to first-parent
// ancestors of any single check-in the way Log (the ancestry walk) is.
//
// Ordering is (mtime DESC, rid DESC), a total order with rid as a true
// tie-break at exact mtime equality — a deliberate improvement over
// canonical fossil, which orders by mtime DESC alone with no tie-break and
// pages using a bare timestamp cursor with a one-second slop, so rows at a
// page boundary can repeat or be skipped there. Pass the last returned
// entry's Cursor back as opts.After to fetch the next page without that
// hazard. The predicate depends on the cursor's mtime component being the
// exact float64 read off the row — see fsltype.Cursor's doc comment for
// why that value must never be reconstructed from a time.Time.
func Timeline(r *repo.Repo, opts TimelineOpts) ([]TimelineEntry, error) {
	if r == nil {
		panic("manifest.Timeline: r must not be nil")
	}
	if !opts.Type.Valid() {
		panic("manifest.Timeline: opts.Type must be a valid EventKind")
	}

	query := "SELECT b.uuid, e.user, e.comment, e.mtime, e.objid, e.type " +
		"FROM event e JOIN blob b ON b.rid = e.objid"

	var conds []string
	var args []any

	if opts.Type != "" {
		conds = append(conds, "e.type = ?")
		args = append(args, string(opts.Type))
	}
	if opts.After.Valid() {
		conds = append(conds, "(e.mtime < ? OR (e.mtime = ? AND b.rid < ?))")
		args = append(args, opts.After.Julian(), opts.After.Julian(), int64(opts.After.RID()))
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY e.mtime DESC, b.rid DESC"
	if opts.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, opts.Limit)
	}

	rows, err := r.DB().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("manifest.Timeline: %w", err)
	}
	defer rows.Close()

	var entries []TimelineEntry
	for rows.Next() {
		if len(entries) >= maxTimelineRows {
			return nil, fmt.Errorf("manifest.Timeline: exceeded max row bound %d", maxTimelineRows)
		}
		// event.user and event.comment are nullable (see Log's comment on
		// the same substitution); event.type is NOT NULL for any row this
		// query can produce (it is set at insert time by crosslink for
		// every event kind).
		var uuid, kind string
		var user, comment sql.NullString
		var mtimeScanned any
		var rid int64
		if err := rows.Scan(&uuid, &user, &comment, &mtimeScanned, &rid, &kind); err != nil {
			return nil, fmt.Errorf("manifest.Timeline: scan: %w", err)
		}
		mtime, ok := db.ScanJulianDay(mtimeScanned)
		if !ok {
			return nil, fmt.Errorf("manifest.Timeline: rid=%d: unexpected mtime type %T", rid, mtimeScanned)
		}
		entry := TimelineEntry{
			LogEntry: LogEntry{
				RID: libfossil.FslID(rid), UUID: uuid, Comment: comment.String,
				User: user.String, Time: libfossil.JulianToTime(mtime),
				Kind: libfossil.EventKind(kind),
			},
			// Cursor carries mtime as the exact float64 just read off this
			// row — not a value re-derived from the Time field above — so
			// handing it back as the next page's After compares equal to
			// this row bit-for-bit. See fsltype.Cursor's doc comment.
			Cursor: libfossil.NewCursor(mtime, libfossil.FslID(rid)),
		}
		// Parents come from plink, which only relates check-in artifacts —
		// leaving Parents empty for every other kind is correct, not a gap.
		if entry.Kind == libfossil.EventKindCheckin {
			entry.Parents = parentUUIDs(r, entry.RID)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("manifest.Timeline: %w", err)
	}
	return entries, nil
}
