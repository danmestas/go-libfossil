package manifest

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/libfossil/db"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/repo"
)

// TimelineOpts configures a repository-wide event enumeration.
type TimelineOpts struct {
	// Type restricts the enumeration to a single event.type code. The zero
	// value means "all kinds" — the canonical `fossil timeline` default.
	Type libfossil.EventKind
	// Before, when non-zero, restricts results to events strictly earlier
	// than the (Before, After) cursor pair: mtime < Before, or mtime ==
	// Before and rid < After. Zero means "start from the newest event".
	Before time.Time
	// After is the cursor's rid tie-break companion; meaningful only
	// alongside a non-zero Before.
	After libfossil.FslID
	// Limit caps the number of rows returned. Zero or negative means
	// unbounded (matches LogOpts.Limit's convention).
	Limit int
}

// maxTimelineRows bounds the enumeration's row loop so that an unbounded
// query (Limit <= 0) against a corrupt or adversarial event table cannot
// hang the process (TigerStyle: every loop needs an explicit bound). A var,
// not a const, so tests can shrink it. No real fossil repository has
// anywhere near this many events.
var maxTimelineRows = 10_000_000

// cursorEpsilonJulian is the tolerance used when comparing a caller-supplied
// Before cursor against the stored e.mtime. A page's Before is built from a
// previously returned LogEntry.Time, which has already been round-tripped
// through at least one lossy float64<->time.Time conversion — and, under
// the ncruces/WASM sqlite driver, a second one: SQLite hands back
// event.mtime as a time.Time via the driver's own REAL conversion, which
// carries its own sub-millisecond noise (observed ~13us) that Go's
// UnixMilli() truncates into a full missed millisecond. A bit-exact cursor
// round trip cannot be guaranteed across drivers, so treat mtimes within
// this window as the same instant for pagination purposes and let the rid
// tie-break do the rest. 10ms comfortably covers the observed drift while
// staying far below any realistic distinct-event time gap.
const cursorEpsilonJulian = 0.010 / 86400.0

// Timeline enumerates the event table newest-first: every event by
// default, or just the events matching opts.Type when set. This is the
// repository-wide enumeration fossil's own `timeline` command performs —
// it does not follow plink at all, so it is not limited to first-parent
// ancestors of any single check-in the way Log (the ancestry walk) is.
//
// Ordering is (mtime DESC, rid DESC) — a deliberate improvement over
// canonical fossil, which orders by mtime DESC alone with no tie-break and
// pages using a bare timestamp cursor with a one-second slop, so rows at a
// page boundary can repeat or be skipped there. Pass the last returned
// entry's Time and RID back as Before/After to fetch the next page without
// that hazard.
func Timeline(r *repo.Repo, opts TimelineOpts) ([]LogEntry, error) {
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
	if !opts.Before.IsZero() {
		conds = append(conds, "(e.mtime < ? OR (e.mtime <= ? AND b.rid < ?))")
		cursor := libfossil.TimeToJulian(opts.Before)
		args = append(args, cursor-cursorEpsilonJulian, cursor+cursorEpsilonJulian, int64(opts.After))
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

	var entries []LogEntry
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
		entry := LogEntry{
			RID: libfossil.FslID(rid), UUID: uuid, Comment: comment.String,
			User: user.String, Time: libfossil.JulianToTime(mtime),
			Kind: libfossil.EventKind(kind),
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
