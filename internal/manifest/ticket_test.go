package manifest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// ticketArtifact builds one D,J,K,U,Z ticket-change artifact -- the card shape
// `fossil ticket add` and `fossil ticket change` emit, and the shape issue #184
// lost in full. deck.Marshal orders the J run and computes the Z card, so the
// bytes are what the parser would accept off the wire.
func ticketArtifact(t *testing.T, ticketUUID string, when time.Time, user string, fields ...deck.TicketField) []byte {
	t.Helper()
	d := &deck.Deck{
		Type: deck.Ticket,
		D:    when,
		J:    fields,
		K:    ticketUUID,
		U:    &user,
	}
	raw, err := d.Marshal()
	if err != nil {
		t.Fatalf("marshal ticket artifact for %s: %v", ticketUUID, err)
	}
	return raw
}

// storeTicketArtifact stores one ticket artifact and returns its rid.
func storeTicketArtifact(t *testing.T, r *repo.Repo, raw []byte) libfossil.FslID {
	t.Helper()
	rid, _, err := blob.Store(r.DB(), raw)
	if err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	return rid
}

// ticketUUID returns a distinct, well-formed 40-hex ticket id for index n.
func ticketUUID(n int) string {
	return strings.Repeat(fmt.Sprintf("%x", n%16), 40)
}

// TestCrosslink_TicketArtifactsProduceEventRows is the regression test whose
// absence let issue #184 ship: every ticket artifact must leave an event row
// behind, and the count must match the artifact count exactly.
//
// Before the fix, crosslinkTicket wrote only the tkt-<uuid> tag and handed the
// event row to a pending-item pass that was a no-op stub, so this count was
// zero while the sweep still reported every artifact linked -- and the tag it
// did write satisfied collectCrosslinkCandidates' tagxref exclusion, hiding
// the artifacts from every later sweep.
func TestCrosslink_TicketArtifactsProduceEventRows(t *testing.T) {
	r := setupTestRepo(t)
	base := time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)

	// Two tickets, each with a create artifact and two changes: enough to
	// exercise the first-artifact and subsequent-artifact branches, and to
	// prove the count is per artifact rather than per ticket.
	type spec struct {
		ticket int
		fields []deck.TicketField
	}
	specs := []spec{
		{1, []deck.TicketField{{Name: "status", Value: "Open"}, {Name: "title", Value: "first bug"}, {Name: "type", Value: "Code_Defect"}}},
		{1, []deck.TicketField{{Name: "status", Value: "Closed"}, {Name: "resolution", Value: "Fixed"}}},
		{1, []deck.TicketField{{Name: "priority", Value: "High"}}},
		{2, []deck.TicketField{{Name: "status", Value: "Open"}, {Name: "title", Value: "second bug"}}},
		{2, []deck.TicketField{{Name: "icomment", Value: "a note"}}},
	}

	for i, s := range specs {
		raw := ticketArtifact(t, ticketUUID(s.ticket), base.Add(time.Duration(i)*time.Minute), "alice", s.fields...)
		storeTicketArtifact(t, r, raw)
	}

	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	var events int
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE type='t'").Scan(&events); err != nil {
		t.Fatalf("count ticket events: %v", err)
	}
	if events != len(specs) {
		t.Fatalf("event rows of type 't' = %d, want %d (one per ticket artifact)", events, len(specs))
	}

	// A second sweep must find nothing left to do. If the tag were still the
	// only marker a ticket artifact wrote, this is where the permanent
	// exclusion of issue #184 would show up as a stable-but-wrong state.
	linked, err := Crosslink(r)
	if err != nil {
		t.Fatalf("second Crosslink: %v", err)
	}
	if linked != 0 {
		t.Fatalf("second Crosslink linked %d artifacts, want 0", linked)
	}
	if err := r.DB().QueryRow("SELECT count(*) FROM event WHERE type='t'").Scan(&events); err != nil {
		t.Fatalf("recount ticket events: %v", err)
	}
	if events != len(specs) {
		t.Fatalf("after a second sweep event rows of type 't' = %d, want %d", events, len(specs))
	}
}
