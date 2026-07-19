package content

import (
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

	current := rid
	for depth := 0; depth < maxAvailabilityChainDepth; depth++ {
		var size int64
		if err := q.QueryRow("SELECT size FROM blob WHERE rid=?", current).Scan(&size); err != nil {
			return false // unknown rid
		}
		if size < 0 {
			return false // phantom
		}

		var srcid int64
		if err := q.QueryRow("SELECT srcid FROM delta WHERE rid=?", current).Scan(&srcid); err != nil {
			return true // not a delta — chain is grounded here
		}
		if srcid == 0 {
			return true
		}
		current = libfossil.FslID(srcid)
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
