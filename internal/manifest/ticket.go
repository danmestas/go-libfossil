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

	schema, err := readTicketSchema(tx)
	if err != nil {
		return err
	}
	human, url := libfossil.HashDigits(tx)

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

// ticketSchema is what fossil's getAllTicketFields learns from the live
// ticket tables (src/tkt.c): the J-card fields each table has, and which of
// the optional bookkeeping columns exist. A repository configures its own
// ticket tables, and older schemas -- fossil's own repository among them --
// lack tkt_ctime, tkt_rid or tkt_user, so none of them is assumed.
type ticketSchema struct {
	tkt, chng   map[string]bool   // J-card fields of ticket and ticketchng
	baseline    map[string]string // ticket field -> its "baseline for" column
	hasCTime    bool              // ticket.tkt_ctime
	hasChngRid  bool              // ticketchng.tkt_rid
	hasChngUser bool              // ticketchng.tkt_user
}

// readTicketSchema reads both ticket tables' columns as getAllTicketFields
// does: tkt_ columns are bookkeeping, a name holding a space is no field,
// and an INTEGER "baseline for NAME" column in ticket records the artifact
// that last assigned field NAME.
func readTicketSchema(q db.Querier) (ticketSchema, error) {
	s := ticketSchema{tkt: map[string]bool{}, chng: map[string]bool{}, baseline: map[string]string{}}
	tkt, err := ticketTableColumns(q, "ticket")
	if err != nil {
		return s, err
	}
	chng, err := ticketTableColumns(q, "ticketchng")
	if err != nil {
		return s, err
	}
	for name := range tkt {
		s.hasCTime = s.hasCTime || name == "tkt_ctime"
		if !strings.HasPrefix(name, "tkt_") && !strings.Contains(name, " ") {
			s.tkt[name] = true
		}
	}
	for name, typ := range tkt {
		field, ok := strings.CutPrefix(name, "baseline for ")
		if ok && typ == "INTEGER" && s.tkt[field] {
			s.baseline[field] = name
		}
	}
	for name := range chng {
		s.hasChngRid = s.hasChngRid || name == "tkt_rid"
		s.hasChngUser = s.hasChngUser || name == "tkt_user"
		if !strings.HasPrefix(name, "tkt_") && !strings.Contains(name, " ") {
			s.chng[name] = true
		}
	}
	return s, nil
}

// ticketTableColumns maps each column of one ticket table to its declared
// type. table must be a literal we control -- PRAGMA takes no parameters.
func ticketTableColumns(q db.Querier, table string) (map[string]string, error) {
	if table != "ticket" && table != "ticketchng" {
		panic("ticketTableColumns: unknown table " + table)
	}
	rows, err := q.Query("SELECT name, type FROM pragma_table_info('" + table + "')")
	if err != nil {
		return nil, fmt.Errorf("ticket columns %s: %w", table, err)
	}
	defer rows.Close()
	cols := make(map[string]string)
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, fmt.Errorf("ticket columns %s scan: %w", table, err)
		}
		cols[name] = typ
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ticket columns %s rows: %w", table, err)
	}
	return cols, nil
}

// sqlIdent quotes a column name as fossil's %w conversion does.
func sqlIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
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
// tables, mirroring fossil's ticket_insert (src/tkt.c). tktID is 0 when
// this is the first artifact of its ticket being replayed, in which case the
// ticket row is created here; the id in force afterwards is returned.
func ticketInsert(tx *db.Tx, d *deck.Deck, rid libfossil.FslID, tktID int64, schema ticketSchema) (int64, error) {
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

	// tkt_ctime takes the first applied artifact's date and never moves
	// again; tkt_mtime advances with every artifact applied.
	set := []string{"tkt_mtime=?1"}
	if schema.hasCTime {
		set = append(set, "tkt_ctime=coalesce(tkt_ctime, ?1)")
	}
	args := []any{mtime}
	for _, a := range ticketAssignments(d, schema.tkt) {
		args = append(args, a.value)
		col := sqlIdent(a.column)
		if a.append {
			set = append(set, fmt.Sprintf("%s=coalesce(%s,'') || ?%d", col, col, len(args)))
			continue
		}
		set = append(set, fmt.Sprintf("%s=?%d", col, len(args)))
		if bsln, ok := schema.baseline[a.column]; ok {
			set = append(set, fmt.Sprintf("%s=%d", sqlIdent(bsln), rid))
		}
	}
	args = append(args, tktID)
	if _, err := tx.Exec(fmt.Sprintf("UPDATE OR REPLACE ticket SET %s WHERE tkt_id=?%d",
		strings.Join(set, ", "), len(args)), args...); err != nil {
		return 0, fmt.Errorf("ticket update: %w", err)
	}
	if err := ticketChngInsert(tx, d, rid, tktID, mtime, schema); err != nil {
		return 0, err
	}
	return tktID, nil
}

// ticketChngInsert writes an artifact's ticketchng row as ticket_insert
// does: only when the artifact sets a ticketchng field or the table records
// the artifact or its user, and with each field both tables share that the
// artifact leaves unset copied from the ticket row it just updated.
func ticketChngInsert(tx *db.Tx, d *deck.Deck, rid libfossil.FslID, tktID int64, mtime float64,
	schema ticketSchema) error {
	cols := []string{"tkt_id", "tkt_mtime"}
	vals := []string{"?", "?"}
	args := []any{tktID, mtime}
	for _, a := range ticketAssignments(d, schema.chng) {
		cols, vals, args = append(cols, sqlIdent(a.column)), append(vals, "?"), append(args, a.value)
	}
	if len(cols) == 2 && !schema.hasChngRid && !schema.hasChngUser {
		return nil
	}
	if schema.hasChngRid {
		cols, vals, args = append(cols, "tkt_rid"), append(vals, "?"), append(args, rid)
	}
	if schema.hasChngUser && d.U != nil {
		cols, vals, args = append(cols, "tkt_user"), append(vals, "?"), append(args, *d.U)
	}
	named := make(map[string]bool, len(d.J))
	for _, f := range d.J {
		named[strings.TrimPrefix(f.Name, "+")] = true
	}
	fromTkt := false
	for _, field := range sortedKeys(schema.chng) {
		if schema.tkt[field] && !named[field] {
			cols, vals = append(cols, sqlIdent(field)), append(vals, sqlIdent(field))
			fromTkt = true
		}
	}
	query := "INSERT INTO ticketchng(" + strings.Join(cols, ", ") + ") VALUES(" + strings.Join(vals, ", ") + ")"
	if fromTkt {
		args = append(args, tktID)
		query = "INSERT INTO ticketchng(" + strings.Join(cols, ", ") + ") SELECT " +
			strings.Join(vals, ", ") + " FROM ticket WHERE tkt_id=?"
	}
	if _, err := tx.Exec(query, args...); err != nil {
		return fmt.Errorf("ticketchng insert: %w", err)
	}
	return nil
}

// sortedKeys returns m's keys in ascending order, fossil's field order.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	title := libfossil.HTMLize(titleCol.String)
	status := libfossil.HTMLize(statusCol.String)
	long, short := libfossil.HashPrefix(d.K, url), libfossil.HashPrefix(d.K, human)

	// A ticket artifact's status card names the new status; every other card
	// is counted but not named. The name is compared verbatim, as fossil's is:
	// a '+status' card appends rather than sets, and counts as a change.
	newStatus := ""
	changes := 0
	for _, f := range d.J {
		if f.Name == "status" {
			newStatus = libfossil.HTMLize(f.Value)
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

func ticketPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
