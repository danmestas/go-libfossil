package manifest

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
)

// TicketWrite is one ticket-change artifact: the fields it sets on ticket
// UUID, by whom and when.
type TicketWrite struct {
	// UUID is the ticket's full 40-digit lowercase id.
	UUID string
	// Fields maps a ticket field to its value; a "+name" key appends the value
	// to the field instead of replacing it. A field with an empty value is
	// left out, as `fossil ticket` leaves it out.
	Fields map[string]string
	User   string
	// Time is the D card; zero means now.
	Time time.Time
}

// WriteTicket stores the ticket-change artifact `fossil ticket add` or
// `fossil ticket set` writes for w (src/tkt.c ticket_cmd) and crosslinks it in
// the same transaction, so the ticket, ticketchng and event rows read back
// at once (ticket_put). It returns the artifact's rid and hash.
//
// Fields are checked against the repository's ticket schema as fossil checks
// them: an unknown field is refused. A private_ field is refused too, because
// fossil conceals its value in the concealed table and this does not.
func WriteTicket(r *repo.Repo, w TicketWrite) (libfossil.FslID, string, error) {
	if r == nil {
		panic("manifest.WriteTicket: r must not be nil")
	}
	if len(w.UUID) != 40 || !isLowerHex(w.UUID) {
		panic("manifest.WriteTicket: w.UUID must be 40 lowercase hex digits")
	}
	if w.User == "" {
		panic("manifest.WriteTicket: w.User must not be empty")
	}
	when := libfossil.CardTimeNow(time.Now())
	if !w.Time.IsZero() {
		when = libfossil.CardTime(w.Time)
	}
	uid, err := r.LocalUID(w.User)
	if err != nil {
		return 0, "", fmt.Errorf("manifest.WriteTicket: %w", err)
	}
	var rid libfossil.FslID
	var uuid string
	err = r.WithReceipt(&db.Receipt{UID: uid}).WithTx(func(tx *db.Tx) error {
		schema, err := readTicketSchema(tx)
		if err != nil {
			return err
		}
		cards, err := ticketCards(schema, w.Fields)
		if err != nil {
			return err
		}
		d := &deck.Deck{Type: deck.Ticket, D: when, J: cards, K: w.UUID, U: deck.User(w.User)}
		rid, uuid, err = storeTicketChange(tx, d)
		return err
	})
	if err != nil {
		return 0, "", fmt.Errorf("manifest.WriteTicket: %w", err)
	}
	return rid, uuid, nil
}

// storeTicketChange stores d and links it as a sweep would link it on
// arrival, then queues it for sync as fossil's ticket_put does.
func storeTicketChange(tx *db.Tx, d *deck.Deck) (libfossil.FslID, string, error) {
	data, err := d.Marshal()
	if err != nil {
		return 0, "", fmt.Errorf("marshal ticket artifact: %w", err)
	}
	// Link what a reader will parse, not the deck it was built from.
	parsed, err := deck.Parse(data)
	if err != nil {
		return 0, "", fmt.Errorf("ticket artifact does not parse: %w", err)
	}
	if parsed.Type != deck.Ticket {
		panic("manifest.storeTicketChange: marshalled deck is not a ticket artifact")
	}
	rid, uuid, err := blob.Store(tx, data)
	if err != nil {
		return 0, "", fmt.Errorf("store ticket artifact: %w", err)
	}
	if err := crosslinkTicket(tx, rid, parsed, content.NewCache(crosslinkCacheBytes)); err != nil {
		return 0, "", err
	}
	// unclustered is handled by blob.Store.
	if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", rid); err != nil {
		return 0, "", fmt.Errorf("ticket unsent: %w", err)
	}
	if _, err := seedcheck.JudgeFilled(tx, []libfossil.FslID{rid}); err != nil {
		return 0, "", err
	}
	return rid, uuid, nil
}

// ticketCards turns fields into J cards, refusing what `fossil ticket` refuses.
// Marshal sorts them; fossil sorts by the name without its '+', which differs
// only where fossil's own parser would reject the result.
func ticketCards(schema ticketSchema, fields map[string]string) ([]deck.TicketField, error) {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	cards := make([]deck.TicketField, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		value := fields[name]
		if value == "" {
			continue
		}
		bare := strings.TrimPrefix(name, "+")
		if _, ok := schema.fields[bare]; !ok {
			return nil, fmt.Errorf("unknown ticket field %q", bare)
		}
		if strings.HasPrefix(bare, "private_") {
			return nil, fmt.Errorf("ticket field %q is private, which is not supported", bare)
		}
		if seen[bare] {
			return nil, fmt.Errorf("ticket field %q is both set and appended", bare)
		}
		seen[bare] = true
		cards = append(cards, deck.TicketField{Name: name, Value: value})
	}
	if len(cards) == 0 {
		return nil, fmt.Errorf("ticket change sets no fields")
	}
	return cards, nil
}

// NewTicketUUID returns a fresh ticket id as fossil makes one: 20 random
// bytes from SQLite, as lowercase hex.
func NewTicketUUID(q db.Querier) (string, error) {
	var id string
	if err := q.QueryRow("SELECT lower(hex(randomblob(20)))").Scan(&id); err != nil {
		return "", fmt.Errorf("new ticket uuid: %w", err)
	}
	if len(id) != 40 {
		panic("manifest.NewTicketUUID: randomblob(20) did not give 40 hex digits")
	}
	return id, nil
}

// ResolveTicket returns the full id of the one ticket whose id starts with
// prefix, as `fossil ticket set` resolves its argument, except that a prefix
// two tickets share is refused rather than resolved to either.
func ResolveTicket(q db.Querier, prefix string) (string, error) {
	prefix = strings.ToLower(prefix)
	if prefix == "" || len(prefix) > 40 || !isLowerHex(prefix) {
		return "", fmt.Errorf("ticket id %q is not a hex prefix", prefix)
	}
	rows, err := q.Query("SELECT tkt_uuid FROM ticket WHERE tkt_uuid GLOB ? LIMIT 2", prefix+"*")
	if err != nil {
		return "", fmt.Errorf("resolve ticket: %w", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("resolve ticket: %w", err)
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("resolve ticket: %w", err)
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("unknown ticket %q", prefix)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("ticket id %q is ambiguous", prefix)
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
