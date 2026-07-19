package content

import (
	"database/sql"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/db"
)

// maxAvailabilityChainDepth bounds the delta-chain walk in IsAvailable.
//
// Real delta chains are orders of magnitude shorter than this; the bound
// exists so that a cyclic delta table — reachable from a corrupt repository
// or from hostile input arriving over the wire during clone — terminates
// instead of hanging the process. Exceeding the bound means the chain cannot
// be expanded, so the artifact is reported unavailable.
const maxAvailabilityChainDepth = 100_000

// IsAvailable reports whether the content of rid can actually be read.
//
// This is the availability predicate, distinct from blob.Exists, which
// answers only whether a row is present. It returns false for a phantom
// (blob.size < 0), false for an unknown rid, and — transitively — false for a
// delta whose chain bottoms out in a phantom, even though every blob row in
// that chain exists. It returns true only when the full chain is grounded in
// readable full-text content.
//
// Ported from Fossil's content_is_available (src/content.c:163). Callers must
// never walk the delta table themselves; that logic lives here alone.
func IsAvailable(q db.Querier, rid libfossil.FslID) bool {
	if q == nil {
		panic("content.IsAvailable: q must not be nil")
	}
	if rid <= 0 {
		return false
	}

	// One statement per chain node, not two. Every SQL round trip here pays
	// a WAL read-lock acquisition, and this walk runs once per F-card of
	// every check-in being crosslinked, over chains that are thousands of
	// nodes deep in a real repository — the round trips, not the work they
	// do, are the cost.
	const step = `SELECT b.size, d.rid IS NOT NULL, d.srcid
	                FROM blob b LEFT JOIN delta d ON d.rid = b.rid
	               WHERE b.rid = ?`

	current := rid
	for depth := 0; depth < maxAvailabilityChainDepth; depth++ {
		var size int64
		var hasDelta bool
		var srcid sql.NullInt64
		if err := q.QueryRow(step, current).Scan(&size, &hasDelta, &srcid); err != nil {
			return false // unknown rid
		}
		if size < 0 {
			return false // phantom
		}
		if !hasDelta {
			return true // chain is grounded in full-text content
		}
		if !srcid.Valid {
			// A delta row whose srcid is NULL says nothing about
			// groundedness, so report unavailable rather than claiming
			// readable content we could not verify.
			return false
		}
		if srcid.Int64 == 0 {
			return true
		}
		current = libfossil.FslID(srcid.Int64)
	}
	return false // bound exceeded — cyclic or pathological chain
}

// AvailableByUUID resolves uuid to its rid and reports whether that
// artifact's content is available, per IsAvailable.
//
// Use this in place of blob.Exists wherever the resolved rid is about to be
// passed to Expand: blob.Exists answers true for a phantom, whose content
// cannot be read.
func AvailableByUUID(q db.Querier, uuid string) (libfossil.FslID, bool) {
	if q == nil {
		panic("content.AvailableByUUID: q must not be nil")
	}
	if uuid == "" {
		panic("content.AvailableByUUID: uuid must not be empty")
	}

	var rid int64
	if err := q.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		return 0, false
	}
	if !IsAvailable(q, libfossil.FslID(rid)) {
		return 0, false
	}
	return libfossil.FslID(rid), true
}
