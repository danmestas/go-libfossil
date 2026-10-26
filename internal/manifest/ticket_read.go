package manifest

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// maxTicketRows bounds the read loops below against a corrupt or adversarial
// table, as maxTimelineRows bounds Timeline's.
const maxTicketRows = 10_000_000

// TicketRow is one row of the ticket table.
type TicketRow struct {
	UUID    string
	Changed time.Time
	// Fields holds every ticket-table field that is not NULL.
	Fields map[string]string
}

// TicketChangeRow is one ticket-change artifact, as `fossil ticket history`
// reads it.
type TicketChangeRow struct {
	Artifact string
	User     string
	Time     time.Time
	// Fields holds the artifact's J cards verbatim: "+name" for an append.
	Fields map[string]string
}

// ListTickets returns every ticket, the most recently changed first. A
// repository without a ticket table has no tickets.
func ListTickets(q db.Querier) ([]TicketRow, error) {
	schema, err := readTicketSchema(q)
	if err != nil {
		return nil, err
	}
	tickets := []TicketRow{}
	if !schema.haveTicket {
		return tickets, nil
	}
	var cols []string
	for name, f := range schema.fields {
		if f.usedBy&usedByTicket != 0 {
			cols = append(cols, name)
		}
	}
	sort.Strings(cols)
	selectCols := "tkt_uuid, tkt_mtime"
	for _, c := range cols {
		selectCols += ", " + sqlIdent(c)
	}
	rows, err := q.Query("SELECT " + selectCols + " FROM ticket ORDER BY tkt_mtime DESC, tkt_uuid")
	if err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if len(tickets) >= maxTicketRows {
			return nil, fmt.Errorf("list tickets: exceeded max row bound %d", maxTicketRows)
		}
		t, err := scanTicket(rows, cols)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	return tickets, nil
}

func scanTicket(rows *sql.Rows, cols []string) (TicketRow, error) {
	var uuid string
	var mtime any
	values := make([]sql.NullString, len(cols))
	dest := []any{&uuid, &mtime}
	for i := range values {
		dest = append(dest, &values[i])
	}
	if err := rows.Scan(dest...); err != nil {
		return TicketRow{}, fmt.Errorf("list tickets: scan: %w", err)
	}
	julian, ok := db.ScanTextJulianDay(mtime)
	if !ok {
		return TicketRow{}, fmt.Errorf(
			"list tickets: %s: unexpected tkt_mtime type %T", uuid, mtime)
	}
	t := TicketRow{UUID: uuid, Changed: libfossil.JulianToTime(julian), Fields: map[string]string{}}
	for i, v := range values {
		if v.Valid {
			t.Fields[cols[i]] = v.String
		}
	}
	return t, nil
}

// TicketChanges returns the change artifacts of the ticket with full id
// ticketUUID in the order fossil applies them: by D card, then by rid.
func TicketChanges(q db.Querier, ticketUUID string) ([]TicketChangeRow, error) {
	if len(ticketUUID) != 40 || !isLowerHex(ticketUUID) {
		panic("manifest.TicketChanges: ticketUUID must be 40 lowercase hex digits")
	}
	rows, err := q.Query(`SELECT x.rid, b.uuid FROM tagxref x
		JOIN tag t ON t.tagid = x.tagid JOIN blob b ON b.rid = x.rid
		WHERE t.tagname = ? ORDER BY x.mtime, x.rid`, "tkt-"+ticketUUID)
	if err != nil {
		return nil, fmt.Errorf("ticket changes: %w", err)
	}
	type artifact struct {
		rid  libfossil.FslID
		uuid string
	}
	var artifacts []artifact
	for rows.Next() {
		if len(artifacts) >= maxTicketRows {
			rows.Close()
			return nil, fmt.Errorf("ticket changes: exceeded max row bound %d", maxTicketRows)
		}
		var a artifact
		if err := rows.Scan(&a.rid, &a.uuid); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ticket changes: scan: %w", err)
		}
		artifacts = append(artifacts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ticket changes: %w", err)
	}
	// Expanded after the rows are closed: content.Expand queries q too.
	changes := make([]TicketChangeRow, 0, len(artifacts))
	for _, a := range artifacts {
		c, ok, err := readTicketChange(q, a.rid, a.uuid, ticketUUID)
		if err != nil {
			return nil, err
		}
		if ok {
			changes = append(changes, c)
		}
	}
	return changes, nil
}

// readTicketChange parses one artifact of a ticket. ok is false for one the
// ticket replay would skip too: not a ticket artifact of this ticket.
func readTicketChange(
	q db.Querier, rid libfossil.FslID, uuid, ticketUUID string,
) (TicketChangeRow, bool, error) {
	data, err := content.Expand(q, rid)
	if err != nil {
		return TicketChangeRow{}, false, fmt.Errorf("ticket change %s: %w", uuid, err)
	}
	d, err := deck.Parse(data)
	if err != nil || d.Type != deck.Ticket || strings.ToLower(d.K) != ticketUUID {
		return TicketChangeRow{}, false, nil
	}
	c := TicketChangeRow{Artifact: uuid, Time: d.D.UTC(), Fields: make(map[string]string, len(d.J))}
	if d.U != nil {
		c.User = *d.U
	}
	for _, j := range d.J {
		c.Fields[j.Name] = j.Value
	}
	return c, true, nil
}
