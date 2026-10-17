package deck

import "time"

type ArtifactType int

const (
	Checkin ArtifactType = iota
	Wiki
	Ticket
	Event
	Cluster
	ForumPost
	Attachment
	Control
	// Submanifest is draft-fossil-submanifests-00's type 9: a slice of a
	// check-in's file set, as F cards (a leaf) or S cards (an interior node).
	// No card assigns it; Parse reassigns a D-less manifest to it (§4.3).
	Submanifest
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
	// S is a tree manifest's or an interior sub-manifest's references, in
	// strictly ascending order of decoded filename
	// (draft-fossil-submanifests-00 §3.6).
	S []SubmanifestCard
	T []TagCard
	// U tracks the check-in user with three distinct states, mirroring
	// fossil's src/manifest.c:1008-1016 U-card handling:
	//   - nil: no U-card in the manifest at all (SQL NULL at crosslink time)
	//   - non-nil, "anonymous": U-card present but empty (resolved at parse
	//     time, matching canonical fossil's own substitution)
	//   - non-nil, non-empty: U-card present with a login name
	// Deliberately *string rather than string: a bare "" cannot represent
	// "absent" and "present-but-empty" at once, which previously collapsed
	// both into the same stored value. See User for constructing literals.
	U *string
	W []byte
	Z string

	// cPresent, nPresent and pPresent record that a C, N or P card was present
	// in the parsed manifest even when its value is empty, so Marshal can
	// re-emit a present-but-empty C/N comment or a bare P card and the artifact
	// round-trips. They are unexported because they matter only to the parse ->
	// marshal round trip: a caller building a Deck sets a non-empty value, which
	// Marshal still emits on its own, and the W card uses nil versus a non-nil
	// empty slice for the same distinction. See §4.2 (empty trailing token) and
	// §4.7.13.
	cPresent bool
	nPresent bool
	pPresent bool
}

type FileCard struct {
	Name    string
	UUID    string
	Perm    string
	OldName string
}

// SubmanifestCard is one S card: the decoded least filename of the slice the
// referenced sub-manifest carries, and that sub-manifest's raw hash.
type SubmanifestCard struct {
	Name string
	UUID string
}

type TagCard struct {
	Type  TagType
	Name  string
	UUID  string
	Value string
	// valuePresent records that a value token was present even when empty, so a
	// parsed `T +x * ` round-trips with its trailing space. Unexported for the
	// same reason as Deck.cPresent: Marshal emits a non-empty Value regardless.
	valuePresent bool
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
