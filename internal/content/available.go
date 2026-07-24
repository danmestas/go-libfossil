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

// AvailabilityCache memoizes IsAvailable across many calls that share delta
// chains, for a caller that knows blob content is immutable for the cache's
// lifetime -- a single crosslink sweep. IsAvailable walks a blob's whole delta
// chain on every call, and crosslink calls it once per F-card of every
// check-in, so the same file blobs -- and the same chain suffixes -- are walked
// again and again; on the Fossil SCM corpus that walk was ~40% of the sweep.
//
// A blob's availability is a property of its chain, and every node above a
// given node in the same chain shares that node's grounding or phantom, so one
// walk decides every node it passes through. The cache records that verdict for
// each rid, and a later walk that reaches an already-decided rid stops there.
// Total walk work over a sweep is then bounded by the number of distinct blobs,
// not by the number of F-card references.
//
// Not safe for concurrent use, and valid only while the delta table and blob
// sizes it walked stay put: create one per sweep, discard it at the end.
type AvailabilityCache struct {
	avail map[libfossil.FslID]bool
}

// NewAvailabilityCache returns an empty availability cache.
func NewAvailabilityCache() *AvailabilityCache {
	return &AvailabilityCache{avail: make(map[libfossil.FslID]bool)}
}

// ByUUID resolves uuid to its rid and reports whether that blob's content is
// available, memoizing the chain walk. Semantics match AvailableByUUID.
func (a *AvailabilityCache) ByUUID(q db.Querier, uuid string) (libfossil.FslID, bool) {
	if a == nil {
		return AvailableByUUID(q, uuid)
	}
	if q == nil {
		panic("content.AvailabilityCache.ByUUID: q must not be nil")
	}
	if uuid == "" {
		panic("content.AvailabilityCache.ByUUID: uuid must not be empty")
	}

	var rid int64
	if err := q.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid); err != nil {
		return 0, false
	}
	if !a.isAvailable(q, libfossil.FslID(rid)) {
		return 0, false
	}
	return libfossil.FslID(rid), true
}

// isAvailable is IsAvailable with memoization. It walks rid's chain until it
// reaches a node whose verdict is already cached, a phantom or unknown rid
// (unavailable), a grounded full-text root (available), a cycle (unavailable),
// or the depth bound (unavailable) -- then records that one verdict for every
// node it walked, since they all share it.
func (a *AvailabilityCache) isAvailable(q db.Querier, rid libfossil.FslID) bool {
	if rid <= 0 {
		return false
	}
	if v, ok := a.avail[rid]; ok {
		return v
	}

	const step = `SELECT b.size, d.rid IS NOT NULL, d.srcid
	                FROM blob b LEFT JOIN delta d ON d.rid = b.rid
	               WHERE b.rid = ?`

	// walked holds every node whose verdict this call decides; result is the
	// verdict they share. seen catches a cycle within this walk (nodes already
	// cached are handled by the memo check and never re-entered).
	var walked []libfossil.FslID
	seen := make(map[libfossil.FslID]struct{})
	result := false
	current := rid
	for depth := 0; depth < maxDeltaChainDepth; depth++ {
		if v, ok := a.avail[current]; ok {
			result = v
			break
		}
		if _, repeat := seen[current]; repeat {
			result = false // cycle — not grounded in anything
			break
		}
		seen[current] = struct{}{}
		walked = append(walked, current)

		var size int64
		var hasDelta bool
		var srcid sql.NullInt64
		if err := q.QueryRow(step, current).Scan(&size, &hasDelta, &srcid); err != nil {
			result = false // unknown rid
			break
		}
		if size < 0 {
			result = false // phantom
			break
		}
		if !hasDelta {
			result = true // chain is grounded in full-text content
			break
		}
		if !srcid.Valid {
			result = false // schema guard: a NULL srcid must not read as grounded
			break
		}
		if srcid.Int64 == 0 {
			result = true // grounded
			break
		}
		current = libfossil.FslID(srcid.Int64)
	}

	for _, n := range walked {
		a.avail[n] = result
	}
	return result
}
