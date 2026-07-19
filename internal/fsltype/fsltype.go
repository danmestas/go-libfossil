// Package fsltype defines shared types used across internal packages.
// These are re-exported by the root libfossil package.
package fsltype

import (
	"math"
	"time"
)

// FslID is a row-id in the blob table (content-addressed artifacts).
type FslID int64

// FslSize represents a blob size; negative values indicate phantom blobs.
type FslSize int64

const (
	// PhantomSize is the sentinel size for phantom (not-yet-received) blobs.
	PhantomSize FslSize = -1

	// FossilApplicationID is the SQLite application_id for Fossil repositories.
	FossilApplicationID int32 = 252006673
)

// EventKind is the type discriminator on the event table's newest-first
// timeline: one of the single-letter codes fossil assigns per row (see
// src/schema.c:303-312). The zero value ("") means "all kinds" — the
// canonical default a bare `fossil timeline` uses when no -t/--type is
// given (src/timeline.c:timeline_cmd(), guarded by `zType && zType[0]!='a'`).
type EventKind string

const (
	// EventKindCheckin is a check-in ('ci').
	EventKindCheckin EventKind = "ci"
	// EventKindTechnote is a technote / event artifact ('e').
	EventKindTechnote EventKind = "e"
	// EventKindForum is a forum post ('f').
	EventKindForum EventKind = "f"
	// EventKindTag is a tag/control artifact ('g').
	EventKindTag EventKind = "g"
	// EventKindTicket is a ticket change ('t').
	EventKindTicket EventKind = "t"
	// EventKindWiki is a wiki page edit ('w').
	EventKindWiki EventKind = "w"
)

// Valid reports whether k is the zero value ("all kinds") or one of the
// six recognized event.type codes.
func (k EventKind) Valid() bool {
	switch k {
	case "", EventKindCheckin, EventKindTechnote, EventKindForum, EventKindTag, EventKindTicket, EventKindWiki:
		return true
	default:
		return false
	}
}

const julianEpoch = 2440587.5

// TimeToJulian converts a time.Time to a Fossil Julian day number.
func TimeToJulian(t time.Time) float64 {
	return julianEpoch + float64(t.UTC().UnixMilli())/(86400.0*1000.0)
}

// JulianToTime converts a Fossil Julian day number to time.Time.
//
// Rounds to the nearest millisecond rather than truncating: a julian value
// produced by TimeToJulian for an exact millisecond carries float64 error
// on the order of 1e-10 days, which a plain int64() truncation can turn
// into a full millisecond of drift when that error lands the value just
// below the true integer. Rounding makes the pair a true round trip
// (TimeToJulian(JulianToTime(m)) == m) for any m that itself came from
// TimeToJulian — the property the Timeline enumeration's composite
// (mtime, rid) pagination cursor depends on.
func JulianToTime(j float64) time.Time {
	millis := int64(math.Round((j - julianEpoch) * 86400.0 * 1000.0))
	return time.UnixMilli(millis).UTC()
}

// Cursor is an opaque pagination token for a Timeline enumeration. Obtain
// one from a returned TimelineEntry's Cursor field and hand it back as the
// next page's cursor to resume immediately after that entry, in the same
// (mtime DESC, rid DESC) order Timeline itself produces. The zero Cursor
// (Valid() == false) means "start from the newest event".
//
// The julian/rid pair is deliberately unexported. A cursor built from
// parts — e.g. re-deriving mtime by round-tripping a TimelineEntry.Time
// back through TimeToJulian — is not guaranteed to equal the row's actual
// stored mtime bit-for-bit, which silently reintroduces skipped or
// duplicated rows at a page boundary (the exact defect this type exists
// to make impossible to construct). Only NewCursor, called by Timeline's
// own row-scanning code with the float64 read directly off the row, may
// produce a valid one.
type Cursor struct {
	julian float64
	rid    FslID
	valid  bool
}

// NewCursor builds a Cursor from a row's exact scanned mtime and rid.
// Callers outside this package's Timeline implementation should not need
// this — obtain a Cursor from a TimelineEntry instead.
func NewCursor(julian float64, rid FslID) Cursor {
	return Cursor{julian: julian, rid: rid, valid: true}
}

// Julian returns the cursor's julian-day mtime component.
func (c Cursor) Julian() float64 { return c.julian }

// RID returns the cursor's rid component.
func (c Cursor) RID() FslID { return c.rid }

// Valid reports whether c was produced by NewCursor (as opposed to being
// a zero-value Cursor{}, which means "no cursor").
func (c Cursor) Valid() bool { return c.valid }
