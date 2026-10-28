package sync

import (
	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
)

// unjudged holds the names of the deltas a pull or clone stored before it
// could read them. Horizon §5.2.3 judges an artifact when it first becomes
// readable, perhaps in a later round, and counts it received only once it is
// judged not a seed: such a delta is left out of the session's count when it
// arrives, and settle counts it at the session's end if it was read and kept.
// A round still counts it as progress, since its bytes were stored.
type unjudged struct {
	names []string
}

// note records uuid, just stored from a card naming deltaSrc as its basis,
// if pass cannot read it yet, and reports whether it did.
func (u *unjudged) note(q db.Querier, pass *receivePass, uuid, deltaSrc string) bool {
	if q == nil || pass == nil {
		panic("sync.unjudged.note: q and pass must not be nil")
	}
	if uuid == "" {
		panic("sync.unjudged.note: uuid must not be empty")
	}
	if deltaSrc == "" || pass.available(q, uuid) {
		return false
	}
	u.names = append(u.names, uuid)
	return true
}

// mark is a position to roll back to when a round's storage rolls back.
func (u *unjudged) mark() int { return len(u.names) }

// rollback forgets what was noted since m: those rows were never kept.
func (u *unjudged) rollback(m int) {
	if m < 0 || m > len(u.names) {
		panic("sync.unjudged.rollback: mark out of range")
	}
	u.names = u.names[:m]
}

// settle reports how many of the noted deltas are readable now and were not
// dropped as seeds, and forgets them all. One still unreadable was never
// judged, so it is not counted.
func (u *unjudged) settle(q db.Querier) (int, error) {
	if q == nil {
		panic("sync.unjudged.settle: q must not be nil")
	}
	if len(u.names) == 0 {
		return 0, nil
	}
	refused, err := seedcheck.RefusedUUIDs(q)
	if err != nil {
		return 0, err
	}
	avail := content.NewAvailabilityCache()
	seen := make(map[string]bool, len(u.names))
	kept := 0
	for _, uuid := range u.names {
		if seen[uuid] || refused[uuid] {
			continue
		}
		seen[uuid] = true
		if _, ok := avail.ByUUID(q, uuid); ok {
			kept++
		}
	}
	u.names = nil
	return kept, nil
}
