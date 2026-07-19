package libfossil

import "github.com/danmestas/libfossil/internal/fsltype"

// FslID is a row-id in the blob table (content-addressed artifacts).
type FslID = fsltype.FslID

// FslSize represents a blob size; negative values indicate phantom blobs.
type FslSize = fsltype.FslSize

const (
	PhantomSize         FslSize = fsltype.PhantomSize
	FossilApplicationID int32   = fsltype.FossilApplicationID
)

// EventKind is the type discriminator on a Timeline entry: one of the
// single-letter codes fossil assigns per event.type row. The zero value
// means "all kinds" — the default Timeline uses when no Type is given.
type EventKind = fsltype.EventKind

const (
	// EventKindCheckin is a check-in ('ci').
	EventKindCheckin EventKind = fsltype.EventKindCheckin
	// EventKindTechnote is a technote / event artifact ('e').
	EventKindTechnote EventKind = fsltype.EventKindTechnote
	// EventKindForum is a forum post ('f').
	EventKindForum EventKind = fsltype.EventKindForum
	// EventKindTag is a tag/control artifact ('g').
	EventKindTag EventKind = fsltype.EventKindTag
	// EventKindTicket is a ticket change ('t').
	EventKindTicket EventKind = fsltype.EventKindTicket
	// EventKindWiki is a wiki page edit ('w').
	EventKindWiki EventKind = fsltype.EventKindWiki
)

// Cursor is an opaque pagination token for Timeline. Obtain one from a
// returned TimelineEntry's Cursor field and pass it back as the next
// TimelineOpts.After to resume enumeration immediately after that entry.
// The zero Cursor means "start from the newest event".
//
// Cursor's representation is deliberately hidden: it cannot be
// constructed from a timestamp and a rid by calling code, only obtained
// from a TimelineEntry the library already produced. See fsltype.Cursor's
// doc comment for why — a hand-built cursor derived from a rounded
// time.Time is not guaranteed to match the row it claims to follow
// exactly, which is what silently reintroduces skipped or duplicated rows
// at a page boundary. LogEntry, Ancestry's result type, has no Cursor
// field at all: Ancestry's pagination is by Start/Limit, so there is no
// valid cursor to obtain from it, and the type reflects that instead of
// documenting it as a hazard.
type Cursor = fsltype.Cursor
