package libfossil

import (
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// TicketOpts is one change to a ticket.
type TicketOpts struct {
	// Fields maps a ticket field (a column of the repository's ticket
	// schema: title, status, type, resolution, comment, ...) to its new
	// value. A "+name" key appends the value to the field instead. A field
	// with an empty value is left out, as `fossil ticket` leaves it out.
	Fields map[string]string
	// User is the change's author, its U card.
	User string
	// Comment, when not empty, is a remark added with the change, stored as
	// the stock ticket forms store one: the icomment field, with login and
	// username set to User and mimetype text/plain.
	Comment string
	// Time is the change's D card; zero means now.
	Time time.Time
}

// Ticket is one ticket as the ticket table holds it after every change.
type Ticket struct {
	UUID   string `json:"uuid"`
	Status string `json:"status"`
	// Changed is the time of the ticket's latest change.
	Changed time.Time `json:"changed"`
	// Fields holds every ticket-table field that has a value.
	Fields map[string]string `json:"fields"`
}

// TicketChange is one change artifact of a ticket.
type TicketChange struct {
	// Artifact is the change artifact's hash.
	Artifact string    `json:"artifact"`
	User     string    `json:"user"`
	Time     time.Time `json:"time"`
	// Comment is the remark the change added, or "".
	Comment string `json:"comment"`
	// Fields holds the change's fields exactly as written, the remark's
	// included: "+name" for an append.
	Fields map[string]string `json:"fields"`
}

// remarkFields are the fields TicketOpts.Comment writes.
var remarkFields = []string{"icomment", "login", "username", "mimetype"}

// TicketAdd creates a ticket, as `fossil ticket add` does, and returns its
// new id and the hash of the artifact that created it. The ticket table
// reflects it when TicketAdd returns, and the artifact is queued to sync.
func (r *Repo) TicketAdd(opts TicketOpts) (ticketUUID, artifact string, err error) {
	ticketUUID, err = manifest.NewTicketUUID(r.inner.DB())
	if err != nil {
		return "", "", fmt.Errorf("libfossil: ticket add: %w", err)
	}
	artifact, err = r.writeTicket(ticketUUID, opts)
	if err != nil {
		return "", "", fmt.Errorf("libfossil: ticket add: %w", err)
	}
	return ticketUUID, artifact, nil
}

// TicketSet changes the ticket whose id is or starts with ticket, as `fossil
// ticket set` does, and returns the hash of the change artifact.
func (r *Repo) TicketSet(ticket string, opts TicketOpts) (artifact string, err error) {
	full, err := manifest.ResolveTicket(r.inner.DB(), ticket)
	if err != nil {
		return "", fmt.Errorf("libfossil: ticket set: %w", err)
	}
	artifact, err = r.writeTicket(full, opts)
	if err != nil {
		return "", fmt.Errorf("libfossil: ticket set: %w", err)
	}
	return artifact, nil
}

func (r *Repo) writeTicket(ticketUUID string, opts TicketOpts) (string, error) {
	if strings.TrimSpace(opts.User) == "" {
		return "", fmt.Errorf("a ticket change needs a user")
	}
	fields := make(map[string]string, len(opts.Fields)+len(remarkFields))
	for name, value := range opts.Fields {
		fields[name] = value
	}
	if opts.Comment != "" {
		for _, name := range remarkFields {
			if fields[name] != "" || fields["+"+name] != "" {
				return "", fmt.Errorf("field %q is written by the comment", name)
			}
		}
		fields["icomment"] = opts.Comment
		fields["login"] = opts.User
		fields["username"] = opts.User
		fields["mimetype"] = "text/plain"
	}
	_, artifact, err := manifest.WriteTicket(r.inner, manifest.TicketWrite{
		UUID: ticketUUID, Fields: fields, User: opts.User, Time: opts.Time,
	})
	return artifact, err
}

// TicketList returns every ticket, the most recently changed first.
func (r *Repo) TicketList() ([]Ticket, error) {
	rows, err := manifest.ListTickets(r.inner.DB())
	if err != nil {
		return nil, fmt.Errorf("libfossil: ticket list: %w", err)
	}
	tickets := make([]Ticket, len(rows))
	for i, t := range rows {
		tickets[i] = Ticket{
			UUID: t.UUID, Status: t.Fields["status"], Changed: t.Changed, Fields: t.Fields,
		}
	}
	return tickets, nil
}

// TicketChanges returns the changes of the ticket whose id is or starts with
// ticket, oldest first: the order fossil applies them in, as `fossil ticket
// history` lists them.
func (r *Repo) TicketChanges(ticket string) ([]TicketChange, error) {
	full, err := manifest.ResolveTicket(r.inner.DB(), ticket)
	if err != nil {
		return nil, fmt.Errorf("libfossil: ticket changes: %w", err)
	}
	rows, err := manifest.TicketChanges(r.inner.DB(), full)
	if err != nil {
		return nil, fmt.Errorf("libfossil: ticket changes: %w", err)
	}
	changes := make([]TicketChange, len(rows))
	for i, c := range rows {
		comment := c.Fields["icomment"]
		if comment == "" {
			comment = c.Fields["+icomment"]
		}
		changes[i] = TicketChange{
			Artifact: c.Artifact, User: c.User, Time: c.Time, Comment: comment, Fields: c.Fields,
		}
	}
	return changes, nil
}

// TimelineSince returns the timeline entries newer than the artifact with
// full hash hash, newest first, in Timeline's order: an entry is newer when
// its time is later, or equal with a higher rid. kinds, when given, keeps only
// entries of those kinds. An empty result is an empty slice, not nil.
//
// Newer means later in the timeline, which is the artifacts' own times, not
// the order they arrived in: an artifact made on a clock behind the given
// artifact's is not newer, whenever it was received.
func (r *Repo) TimelineSince(hash string, kinds ...EventKind) ([]TimelineEntry, error) {
	for _, k := range kinds {
		if k == "" || !k.Valid() {
			return nil, fmt.Errorf("libfossil: timeline since: unknown event kind %q", k)
		}
	}
	since, err := manifest.TimelineCursor(r.inner.DB(), hash)
	if err != nil {
		return nil, fmt.Errorf("libfossil: timeline since: %w", err)
	}
	entries, err := manifest.Timeline(r.inner, manifest.TimelineOpts{
		Kinds: []fsltype.EventKind(kinds), Since: since,
	})
	if err != nil {
		return nil, fmt.Errorf("libfossil: timeline since: %w", err)
	}
	return timelineEntries(entries), nil
}
