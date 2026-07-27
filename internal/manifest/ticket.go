package manifest

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ticketRebuildEntry re-derives every row one ticket owns -- its ticket row,
// its ticketchng rows and one event row per change artifact -- by replaying
// every artifact carrying the tkt-<uuid> tag in mtime order. It is fossil's
// ticket_rebuild_entry (src/ticket.c), which fossil defers to the end of a
// crosslink run through a pending_tkt table.
//
// We run it inline instead, inside the caller's transaction, because the
// deferral is what issue #184 turned into silent data loss: crosslinkTicket
// wrote the tkt- tag immediately and left the event row to a later pass, and
// collectCrosslinkCandidates excludes any blob that already has a tagxref row
// with its own rid as srcid. A ticket artifact whose deferred half never ran
// was therefore both eventless and permanently unselectable, with nothing
// logged. Running the rebuild here makes the tag and the event row one unit of
// work: they commit together or not at all, so no partial state can exclude
// the artifact from a later sweep.
//
// Replaying the whole tag set per artifact rather than once per run is what
// buys that atomicity, and it is also what makes the result independent of
// arrival order -- a change artifact that arrives before the create artifact
// it amends produces the same final rows either way, because the last replay
// to run rewrites all of them.
//
// Cost, measured, so that nobody has to rediscover it from a profile: this is
// quadratic in one ticket's artifact count -- 50 artifacts 47ms, 200 431ms,
// 400 1.5s -- and flat in everything else, because a ticket's replay set is
// only its own artifacts. Fossil's own repository averages about two artifacts
// per ticket, so the real figure there is noise.
//
// The cheaper shape is to collect ticket uuids during a batch and rebuild each
// one once before the transaction commits; that stays atomic and would collapse
// the quadratic term. It is deliberately not what this does. It requires both
// linkBatch and cascadeLinker.flush to remember to make that call, and a
// forgotten call reintroduces issue #184 exactly: rows missing, nothing logged,
// test-integrity clean, and the tag already written to keep the artifact hidden
// from every later sweep. That is the second bug of the shape this package has
// shipped (see also #180). If you are here to optimize a repository with one
// hot ticket, move the call, do not drop it -- and prove it with
// TestCrosslink_TicketArtifactsProduceEventRows and TestFossilBinaryTicketParity.
func ticketRebuildEntry(tx *db.Tx, cache *content.Cache, ticketUUID string) error {
	if tx == nil {
		panic("ticketRebuildEntry: tx must not be nil")
	}
	if ticketUUID == "" {
		panic("ticketRebuildEntry: ticketUUID must not be empty")
	}

	var tagID int64
	if err := tx.QueryRow("SELECT tagid FROM tag WHERE tagname=?", "tkt-"+ticketUUID).Scan(&tagID); err != nil {
		return fmt.Errorf("ticket tagid: %w", err)
	}

	tktCols, err := ticketColumns(tx, "ticket")
	if err != nil {
		return err
	}
	chngCols, err := ticketColumns(tx, "ticketchng")
	if err != nil {
		return err
	}
	human, url := ticketHashDigits(tx)

	// The ticket row is rebuilt from scratch so a replay reflects only the
	// artifacts that exist now; its ticketchng rows go with it, since they are
	// keyed by the tkt_id this DELETE drops. The event rows need no delete:
	// event.objid is the primary key, so each replay REPLACEs its own row.
	if _, err := tx.Exec("DELETE FROM ticketchng WHERE tkt_id IN (SELECT tkt_id FROM ticket WHERE tkt_uuid=?)", ticketUUID); err != nil {
		return fmt.Errorf("ticket clear ticketchng: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM ticket WHERE tkt_uuid=?", ticketUUID); err != nil {
		return fmt.Errorf("ticket clear ticket: %w", err)
	}

	// mtime is fossil's replay order; rid breaks exact ties, which fossil
	// leaves to whatever order SQLite happens to scan in. Two syncs delivering
	// the same artifacts in different orders must produce the same rows.
	rows, err := tx.Query("SELECT rid FROM tagxref WHERE tagid=? ORDER BY mtime, rid", tagID)
	if err != nil {
		return fmt.Errorf("ticket replay query: %w", err)
	}
	var rids []libfossil.FslID
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			return fmt.Errorf("ticket replay scan: %w", err)
		}
		rids = append(rids, libfossil.FslID(rid))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("ticket replay rows: %w", err)
	}
	rows.Close()

	var tktID int64 // 0 until the first replayable artifact creates the row.
	for _, rid := range rids {
		data, err := expandManifestBytes(tx, cache, rid)
		if err != nil {
			continue // blob not expandable yet; a later sweep replays it
		}
		d, err := deck.Parse(data)
		if err != nil || d.Type != deck.Ticket || d.K != ticketUUID {
			continue
		}
		isNew := tktID == 0
		tktID, err = ticketInsert(tx, d, rid, tktID, tktCols, chngCols)
		if err != nil {
			return err
		}
		if err := ticketEvent(tx, rid, d, isNew, tagID, human, url); err != nil {
			return err
		}
	}
	return nil
}

// ticketColumns returns the columns of one ticket table a J-card may write.
// Fossil's getAllTicketFields reads them from the live schema and skips the
// tkt_-prefixed bookkeeping columns it manages itself, because a repository
// may configure its own ticket fields; we do the same rather than assume the
// default schema. table must be a literal we control -- PRAGMA takes no
// parameters.
func ticketColumns(q db.Querier, table string) (map[string]bool, error) {
	if table != "ticket" && table != "ticketchng" {
		panic("ticketColumns: unknown table " + table)
	}
	rows, err := q.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, fmt.Errorf("ticket columns %s: %w", table, err)
	}
	defer rows.Close()

	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name string
		var typ, dfltValue any
		var notNull, pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			return nil, fmt.Errorf("ticket columns %s scan: %w", table, err)
		}
		if strings.HasPrefix(name, "tkt_") {
			continue
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ticket columns %s rows: %w", table, err)
	}
	return cols, nil
}

// ticketHashDigits returns the two hyperlink hash lengths fossil's %S and %!S
// conversions use in a ticket event comment: the human length (the hash-digits
// setting, default 10, clamped to 6..40) and the url length (human+6, floored
// at 16). Mirrors fossil's hashDigits().
func ticketHashDigits(q db.Querier) (human, url int) {
	human = 10
	var v int
	if q.QueryRow("SELECT value FROM config WHERE name='hash-digits'").Scan(&v) == nil && v != 0 {
		human = v
	}
	if human < 6 {
		human = 6
	}
	if human > 40 {
		human = 40
	}
	url = human + 6
	if url < 16 {
		url = 16
	}
	return human, url
}

// ticketAssignment is one J-card resolved against a ticket table's columns.
type ticketAssignment struct {
	column string
	value  string
	append bool // the card was +name: concatenate onto the stored value
}

// ticketAssignments resolves a ticket artifact's J-cards against cols, keeping
// wire order and dropping cards that name no column. A leading '+' selects
// append semantics and is not part of the column name. A column named twice --
// which the ascending-J-run rule permits only as `name` plus `+name` -- keeps
// the first card, so the statement built from this never assigns one column
// twice.
func ticketAssignments(d *deck.Deck, cols map[string]bool) []ticketAssignment {
	var out []ticketAssignment
	seen := make(map[string]bool, len(d.J))
	for _, f := range d.J {
		name, isAppend := strings.CutPrefix(f.Name, "+")
		if !cols[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, ticketAssignment{column: name, value: f.Value, append: isAppend})
	}
	return out
}

// ticketInsert applies one ticket artifact to the ticket and ticketchng
// tables, mirroring fossil's ticket_insert (src/ticket.c). tktID is 0 when
// this is the first artifact of its ticket being replayed, in which case the
// ticket row is created here; the id in force afterwards is returned.
func ticketInsert(
	tx *db.Tx,
	d *deck.Deck,
	rid libfossil.FslID,
	tktID int64,
	tktCols, chngCols map[string]bool,
) (int64, error) {
	mtime := libfossil.TimeToJulian(d.D)

	if tktID == 0 {
		// tkt_ctime is the creating artifact's date and never moves again;
		// tkt_mtime is set by the UPDATE below, so every replay advances it to
		// the latest artifact applied.
		res, err := tx.Exec(
			"INSERT INTO ticket(tkt_uuid, tkt_mtime, tkt_ctime) VALUES(?, 0, ?)", d.K, mtime)
		if err != nil {
			return 0, fmt.Errorf("ticket insert: %w", err)
		}
		tktID, err = res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("ticket insert id: %w", err)
		}
	}

	set := []string{"tkt_mtime=?"}
	args := []any{mtime}
	for _, a := range ticketAssignments(d, tktCols) {
		if a.append {
			set = append(set, fmt.Sprintf("%s=coalesce(%s,'') || ?", a.column, a.column))
		} else {
			set = append(set, a.column+"=?")
		}
		args = append(args, a.value)
	}
	args = append(args, tktID)
	if _, err := tx.Exec("UPDATE ticket SET "+strings.Join(set, ", ")+" WHERE tkt_id=?", args...); err != nil {
		return 0, fmt.Errorf("ticket update: %w", err)
	}

	cols := []string{"tkt_id", "tkt_rid", "tkt_mtime", "tkt_user"}
	vals := []any{tktID, rid, mtime, d.U}
	for _, a := range ticketAssignments(d, chngCols) {
		cols = append(cols, a.column)
		vals = append(vals, a.value)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
	if _, err := tx.Exec(
		"INSERT INTO ticketchng("+strings.Join(cols, ", ")+") VALUES("+placeholders+")", vals...,
	); err != nil {
		return 0, fmt.Errorf("ticketchng insert: %w", err)
	}
	return tktID, nil
}

// ticketEvent writes the timeline row for one ticket artifact, mirroring
// fossil's manifest_ticket_event (src/manifest.c). The wording is fossil's,
// verified against `fossil rebuild` output for each of its three branches: the
// creating artifact, a later artifact that sets a status, and a later artifact
// that does not.
//
// The title and status it renders come from the ticket table as it stands
// after this artifact was applied, not from the artifact -- a change artifact
// carries only the fields it changes, so its own cards rarely include either.
func ticketEvent(
	tx *db.Tx,
	rid libfossil.FslID,
	d *deck.Deck,
	isNew bool,
	tagID int64,
	human, url int,
) error {
	var titleCol, statusCol sql.NullString
	if err := tx.QueryRow(
		"SELECT title, status FROM ticket WHERE tkt_uuid=?", d.K,
	).Scan(&titleCol, &statusCol); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("ticket event state: %w", err)
	}
	title := ticketHTML.Replace(titleCol.String)
	status := ticketHTML.Replace(statusCol.String)
	long, short := ticketShorten(d.K, url), ticketShorten(d.K, human)

	// A ticket artifact's status card names the new status; every other card
	// is counted but not named. The name is compared verbatim, as fossil's is:
	// a '+status' card appends rather than sets, and counts as a change.
	newStatus := ""
	changes := 0
	for _, f := range d.J {
		if f.Name == "status" {
			newStatus = ticketHTML.Replace(f.Value)
			continue
		}
		changes++
	}

	var comment, brief string
	switch {
	case isNew:
		comment = fmt.Sprintf("New ticket [%s|%s] <i>%s</i>.", long, short, title)
		brief = fmt.Sprintf("New ticket [%s|%s].", long, short)
	case newStatus != "":
		comment = fmt.Sprintf("%s ticket [%s|%s]: <i>%s</i>", newStatus, long, short, title)
		if changes > 0 {
			comment += fmt.Sprintf(" plus %d other change%s", changes, ticketPlural(changes))
		}
		brief = fmt.Sprintf("%s ticket [%s|%s].", newStatus, long, short)
	default:
		comment = fmt.Sprintf("Ticket [%s|%s] <i>%s</i> status still %s with %d other change%s",
			long, short, title, status, changes, ticketPlural(changes))
		brief = fmt.Sprintf("Ticket [%s|%s]: %d change%s", long, short, changes, ticketPlural(changes))
	}

	if _, err := tx.Exec(
		"REPLACE INTO event(type, tagid, mtime, objid, user, comment, brief) VALUES('t', ?, ?, ?, ?, ?, ?)",
		tagID, libfossil.TimeToJulian(d.D), rid, d.U, comment, brief,
	); err != nil {
		return fmt.Errorf("ticket event: %w", err)
	}
	return nil
}

// ticketHTML escapes exactly what fossil's %h conversion escapes, in the same
// forms. Go's html.EscapeString agrees on every character except the double
// quote, which it writes as &#34; where fossil writes &quot;.
var ticketHTML = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)

// ticketShorten truncates a hash for display, matching fossil's %S and %!S
// conversions: shorter values pass through unchanged.
func ticketShorten(hash string, n int) string {
	if len(hash) > n {
		return hash[:n]
	}
	return hash
}

func ticketPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
