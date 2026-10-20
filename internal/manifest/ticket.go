package manifest

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ticketRebuildEntry re-derives every row one ticket owns -- its ticket row,
// its ticketchng rows and one event row per change artifact -- by replaying
// every artifact carrying the tkt-<uuid> tag in mtime order. It is fossil's
// ticket_rebuild_entry (src/tkt.c), which fossil defers to the end of a
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

	schema, err := readTicketSchema(tx)
	if err != nil {
		return err
	}
	// Without a ticket table fossil derives nothing for a ticket, not even
	// its timeline rows: ticket_rebuild_entry returns before replaying.
	if !schema.haveTicket {
		return nil
	}
	human, url := ticketHashDigits(tx)

	// The ticket row is rebuilt from scratch so a replay reflects only the
	// artifacts that exist now; its ticketchng rows go with it, since they are
	// keyed by the tkt_id this DELETE drops. The event rows need no delete:
	// event.objid is the primary key, so each replay REPLACEs its own row.
	if schema.haveChng {
		if _, err := tx.Exec("DELETE FROM ticketchng WHERE tkt_id IN (SELECT tkt_id FROM ticket WHERE tkt_uuid=?)", ticketUUID); err != nil {
			return fmt.Errorf("ticket clear ticketchng: %w", err)
		}
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
		tktID, err = ticketInsert(tx, d, rid, tktID, schema)
		if err != nil {
			return err
		}
		if err := ticketEvent(tx, rid, d, isNew, tagID, human, url); err != nil {
			return err
		}
	}
	return nil
}

// Which tables a ticket field is stored in, as fossil's USEDBY_ bits.
const (
	usedByTicket uint8 = 1 << iota
	usedByTicketChng
	usedByBoth = usedByTicket | usedByTicketChng
)

// ticketField is one column a J-card may write.
type ticketField struct {
	usedBy uint8
	// baseline names the ticket table's "baseline for <field>" INTEGER
	// column, which records the rid that last assigned the field; "" if none.
	baseline string
}

// ticketSchema is the shape of a repository's ticket tables. Fossil lets a
// repository configure its own ticket fields, and treats the ticket table,
// ticket.tkt_ctime, the ticketchng table, ticketchng.tkt_rid and
// ticketchng.tkt_user as optional; fossil's own repository has no tkt_user.
type ticketSchema struct {
	haveTicket, haveCTime               bool
	haveChng, haveChngRid, haveChngUser bool
	fields                              map[string]ticketField
}

// readTicketSchema reads the live ticket schema, mirroring fossil's
// getAllTicketFields (src/tkt.c): tkt_-prefixed columns are bookkeeping, not
// fields, and a name containing a space is never a field.
func readTicketSchema(q db.Querier) (ticketSchema, error) {
	s := ticketSchema{fields: make(map[string]ticketField)}
	baselines := make(map[string]bool)
	err := forEachColumn(q, "ticket", func(name, typ string) {
		s.haveTicket = true
		switch {
		case strings.HasPrefix(name, "tkt_"):
			s.haveCTime = s.haveCTime || name == "tkt_ctime"
		case strings.HasPrefix(name, "baseline for "):
			if typ == "INTEGER" {
				baselines[name] = true
			}
		case strings.Contains(name, " "):
		default:
			s.fields[name] = ticketField{usedBy: usedByTicket}
		}
	})
	if err != nil {
		return ticketSchema{}, err
	}
	for name, f := range s.fields {
		if b := "baseline for " + name; baselines[b] {
			f.baseline = b
			s.fields[name] = f
		}
	}
	err = forEachColumn(q, "ticketchng", func(name, _ string) {
		s.haveChng = true
		switch {
		case strings.HasPrefix(name, "tkt_"):
			s.haveChngRid = s.haveChngRid || name == "tkt_rid"
			s.haveChngUser = s.haveChngUser || name == "tkt_user"
		case strings.Contains(name, " "):
		default:
			f := s.fields[name]
			f.usedBy |= usedByTicketChng
			s.fields[name] = f
		}
	})
	if err != nil {
		return ticketSchema{}, err
	}
	return s, nil
}

// forEachColumn calls fn with the name and declared type of each column of
// table, and not at all when the table does not exist. table must be a literal
// we control -- PRAGMA takes no parameters.
func forEachColumn(q db.Querier, table string, fn func(name, typ string)) error {
	if table != "ticket" && table != "ticketchng" {
		panic("forEachColumn: unknown table " + table)
	}
	rows, err := q.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("ticket columns %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var typ sql.NullString
		var dfltValue any
		var notNull, pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("ticket columns %s scan: %w", table, err)
		}
		fn(name, typ.String)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ticket columns %s rows: %w", table, err)
	}
	return nil
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

// ticketInsert applies one ticket artifact to the ticket and ticketchng
// tables, mirroring fossil's ticket_insert (src/tkt.c) column for column.
// tktID is 0 when this is the first artifact of its ticket being replayed, in
// which case the ticket row is created here; the id in force afterwards is
// returned.
func ticketInsert(
	tx *db.Tx,
	d *deck.Deck,
	rid libfossil.FslID,
	tktID int64,
	schema ticketSchema,
) (int64, error) {
	if !schema.haveTicket {
		panic("ticketInsert: no ticket table")
	}
	mtime := libfossil.TimeToJulian(d.D)

	if tktID == 0 {
		res, err := tx.Exec("INSERT INTO ticket(tkt_uuid, tkt_mtime) VALUES(?, 0)", d.K)
		if err != nil {
			return 0, fmt.Errorf("ticket insert: %w", err)
		}
		tktID, err = res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("ticket insert id: %w", err)
		}
	}

	// tkt_mtime advances to every artifact applied; tkt_ctime, where the
	// schema has it, is the first artifact's date and never moves again.
	set := []string{"tkt_mtime=?"}
	args := []any{mtime}
	if schema.haveCTime {
		set = append(set, "tkt_ctime=coalesce(tkt_ctime, ?)")
		args = append(args, mtime)
	}
	var chngCols []string
	var chngVals []any
	used := make(map[string]bool, len(d.J))
	for _, j := range d.J {
		name, isAppend := strings.CutPrefix(j.Name, "+")
		f, ok := schema.fields[name]
		if !ok {
			continue
		}
		// A column named twice -- which the ascending-J-run rule permits only
		// as `name` plus `+name` -- is assigned twice in the UPDATE, where
		// SQLite keeps the rightmost assignment as fossil's statement does,
		// but inserted once, since an INSERT may not name a column twice.
		first := !used[name]
		used[name] = true
		if f.usedBy&usedByTicket != 0 {
			if isAppend {
				set = append(set, fmt.Sprintf(`%s=coalesce(%s,'') || ?`, sqlIdent(name), sqlIdent(name)))
				args = append(args, j.Value)
			} else {
				set = append(set, sqlIdent(name)+"=?")
				args = append(args, j.Value)
				if f.baseline != "" {
					set = append(set, sqlIdent(f.baseline)+"=?")
					args = append(args, rid)
				}
			}
		}
		if f.usedBy&usedByTicketChng != 0 && first {
			chngCols = append(chngCols, sqlIdent(name))
			chngVals = append(chngVals, j.Value)
		}
	}
	args = append(args, tktID)
	if _, err := tx.Exec(
		"UPDATE OR REPLACE ticket SET "+strings.Join(set, ", ")+" WHERE tkt_id=?", args...,
	); err != nil {
		return 0, fmt.Errorf("ticket update: %w", err)
	}

	// Fossil writes a change row only when there is something to put in it
	// beyond the id and time: a ticketchng field this artifact sets, or one
	// of the optional bookkeeping columns.
	if len(chngCols) == 0 && !schema.haveChngRid && !schema.haveChngUser {
		return tktID, nil
	}
	if schema.haveChngRid {
		chngCols = append(chngCols, "tkt_rid")
		chngVals = append(chngVals, rid)
	}
	if schema.haveChngUser && d.U != nil {
		chngCols = append(chngCols, "tkt_user")
		chngVals = append(chngVals, *d.U)
	}
	// A field stored in both tables that this artifact leaves alone is copied
	// into the change row from the ticket row as the UPDATE left it.
	var fromTkt []string
	for name, f := range schema.fields {
		if f.usedBy == usedByBoth && !used[name] {
			fromTkt = append(fromTkt, sqlIdent(name))
		}
	}
	sort.Strings(fromTkt)

	cols := append([]string{"tkt_id", "tkt_mtime"}, chngCols...)
	vals := append([]any{tktID, mtime}, chngVals...)
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(vals)), ", ")
	var err error
	if len(fromTkt) == 0 {
		_, err = tx.Exec(
			"INSERT INTO ticketchng("+strings.Join(cols, ", ")+") VALUES("+placeholders+")", vals...)
	} else {
		cols = append(cols, fromTkt...)
		_, err = tx.Exec(
			"INSERT INTO ticketchng("+strings.Join(cols, ", ")+") SELECT "+placeholders+", "+
				strings.Join(fromTkt, ", ")+" FROM ticket WHERE tkt_id=?", append(vals, tktID)...)
	}
	if err != nil {
		return 0, fmt.Errorf("ticketchng insert: %w", err)
	}
	return tktID, nil
}

// sqlIdent quotes a column name read from the schema, as fossil's %w
// conversion inside double quotes does.
func sqlIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
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
		tagID, libfossil.TimeToTextJulian(d.D), rid, d.U, comment, brief,
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
