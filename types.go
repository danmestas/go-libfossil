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
