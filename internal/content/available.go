package content

import (
	"database/sql"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// maxDeltaChainDepth bounds every delta-chain walk in the package: the two
// read-path walks, IsAvailable here and walkDeltaChain in content.go, and the
// write-path walk deltifyBreaksLoop in deltify.go. Each stops after visiting
// this many nodes. One concept, one bound: the rationale below is the same for
// all three, since they walk the same delta table over the same chains.
//
// A chain cannot be longer than the number of stored versions of one file.
// The deepest chain in the Fossil SCM repository, the largest corpus this
// package is measured against, is 2,546 nodes; the average is 469. This bound
// is roughly six times the observed maximum, so a chain that reaches it is
// corrupt or hostile rather than merely large. Reaching it is a safe failure:
// IsAvailable reports the content unavailable, which defers the artifact, and
// walkDeltaChain returns an error rather than a partial chain.
//
// The bound is a backstop, not the defence. Termination on a cyclic delta
// table — reachable from a corrupt repository, or from a peer that plants one
// during clone — comes from the visited-set check in each walk, which stops at
// the first repeat. Without that, this constant is the number of queries an
// attacker buys per call, and both walks are wire-reachable: IsAvailable runs
// once per F-card of every check-in being crosslinked.
const maxDeltaChainDepth = 16_384

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

	// A cycle ends the walk at the first repeat rather than at
	// maxDeltaChainDepth. The difference is what a peer who plants a
	// two-node cycle costs us: two queries instead of the bound.
	seen := make(map[libfossil.FslID]struct{})

	current := rid
	for depth := 0; depth < maxDeltaChainDepth; depth++ {
		if _, repeat := seen[current]; repeat {
			return false // cycle — the chain is not grounded in anything
		}
		seen[current] = struct{}{}

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
		// delta.srcid is NOT NULL in the schema, so Valid is always true
		// here. Checking it costs nothing and keeps a schema change from
		// turning into a NULL scanned as zero, which reads as "grounded".
		if !srcid.Valid {
			return false
		}
		if srcid.Int64 == 0 {
			return true
		}
		current = libfossil.FslID(srcid.Int64)
	}
	return false // bound exceeded — pathological chain
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
