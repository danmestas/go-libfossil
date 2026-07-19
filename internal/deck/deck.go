package deck

import "time"

type ArtifactType int

const (
	Checkin    ArtifactType = iota
	Wiki
	Ticket
	Event
	Cluster
	ForumPost
	Attachment
	Control
)

type Deck struct {
	Type ArtifactType
	A    *AttachmentCard
	B    string
	C    string
	D    time.Time
	E    *EventCard
	F    []FileCard
	G    string
	H    string
	I    string
	J    []TicketField
	K    string
	L    string
	M    []string
	N    string
	P    []string
	Q    []CherryPick
	R    string
	T    []TagCard
	// U tracks the check-in user with three distinct states, mirroring
	// fossil's src/manifest.c:1008-1016 U-card handling:
	//   - nil: no U-card in the manifest at all (SQL NULL at crosslink time)
	//   - non-nil, "anonymous": U-card present but empty (resolved at parse
	//     time, matching canonical fossil's own substitution)
	//   - non-nil, non-empty: U-card present with a login name
	// Deliberately *string rather than string: a bare "" cannot represent
	// "absent" and "present-but-empty" at once, which previously collapsed
	// both into the same stored value. See User for constructing literals.
	U    *string
	W    []byte
	Z    string
}

type FileCard struct {
	Name    string
	UUID    string
	Perm    string
	OldName string
}

type TagCard struct {
	Type  TagType
	Name  string
	UUID  string
	Value string
}

type TagType byte

const (
	TagSingleton   TagType = '+'
	TagPropagating TagType = '*'
	TagCancel      TagType = '-'
)

type CherryPick struct {
	IsBackout bool
	Target    string
	Baseline  string
}

type AttachmentCard struct {
	Filename string
	Target   string
	Source   string
}

type EventCard struct {
	Date time.Time
	UUID string
}

type TicketField struct {
	Name  string
	Value string
}

// User returns a pointer to s for constructing Deck.U. Go forbids taking
// the address of a string literal directly, so composite literals go
// through this instead of Deck{U: &s}. Deliberately *string rather than a
// named struct type: passing *string straight to database/sql lets a nil
// argument bind SQL NULL and a non-nil one deref to its value automatically
// (see Deck.U), so crosslink/rebuild call sites need no special-casing.
func User(s string) *string {
	return &s
}
