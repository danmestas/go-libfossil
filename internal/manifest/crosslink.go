package manifest

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/tag"
)

// crosslinkCancelCheckStride is how many candidates the sweep processes between
// context-cancellation checks. The sweep is the only phase of a clone that
// touches every artifact in one uninterruptible call, so it must observe the
// deadline; checking every candidate would pay a channel read per artifact, so
// the check is batched. The value bounds cancellation latency to this many
// candidates' worth of work.
const crosslinkCancelCheckStride = 256

// attachTargetTypeName maps attachment target type codes to human-readable names.
// Used by crosslinkAttachment and updateAttachmentComments.
var attachTargetTypeName = map[byte]string{
	'w': "wiki page",
	't': "ticket",
	'e': "tech note",
}

// crosslinkCacheBytes bounds the expanded content one crosslink run keeps live
// -- a whole-repository sweep, or one phantom-fill cascade (see
// cascadeLinker). A miss costs throughput, not correctness: the walk simply
// continues further back toward the chain root.
//
// Candidates are visited in delta-chain order (see deltaChainOrder), not
// ascending rid: every base a candidate needs is expanded at most one
// candidate-visit earlier, never arbitrarily far in the future. That bounds
// the working set by how many chains are in flight at once -- how many
// distinct files or manifests are being interleaved -- rather than by
// repository size, so this budget only has to cover that concurrency, not
// the whole repository expanded (8 GiB for the Fossil SCM repository under
// the old ascending-rid order this replaced).
//
// This number is the process's memory ceiling, not just the cache's, and the
// two are not close. Cloning the Fossil SCM repository, each byte of budget
// costs roughly five bytes of peak RSS: GOGC=100 doubles it into the heap
// goal, and the free spans the sweep's own churn leaves behind are returned to
// the OS by a scavenger paced off GC cycles, of which a faster sweep completes
// fewer. Measured end to end (67.6k blobs, cache budget against wall time and
// peak RSS):
//
//	 32 MiB   369.6 s    318 MB
//	 64 MiB   292.3 s    447 MB
//	128 MiB   249.4 s    812 MB
//	256 MiB   205.7 s   1473 MB
//
// One-shot rebuilds retain the 256 MiB cache budget. Receive-time linking has
// a separate, smaller budget because its session also retains bounded
// coordination metadata until Finalize.
const crosslinkCacheBytes = 256 << 20

// ensureForumPostTable creates forumpost if a prior `fossil rebuild` (or a
// repository that never had one) left it absent. Canonical fossil creates
// this table lazily -- only once a forum artifact needs it -- and drops it
// during rebuild along with the rest of the on-demand schema when nothing
// populated it. Schema matches db.schemaRepo2's forumpost definition
// exactly, since the two must produce byte-identical tables whichever one
// creates it.
func ensureForumPostTable(q db.Querier) error {
	if q == nil {
		panic("manifest.ensureForumPostTable: q must not be nil")
	}
	_, err := q.Exec(`
		CREATE TABLE IF NOT EXISTS forumpost(
		  fpid INTEGER PRIMARY KEY,
		  froot INT,
		  fprev INT,
		  firt INT,
		  fmtime REAL
		);
		CREATE INDEX IF NOT EXISTS forumpost_froot ON forumpost(froot);
	`)
	if err != nil {
		return fmt.Errorf("ensure forumpost table: %w", err)
	}
	return nil
}

// nonArtifactTable records blobs a sweep has already expanded and found to
// hold no Fossil artifact at all -- ordinary file content, which is most of
// any repository. See ensureNonArtifactTable.
const nonArtifactTable = "crosslink_nonartifact"

// ensureNonArtifactTable creates the sweep's already-examined set.
//
// The candidate query below finds blobs with none of the derived rows a
// crosslink writes. Ordinary file content never gains those rows however
// often it is examined, so without a record of the verdict every sweep
// re-expands and re-parses every file blob in the repository: 39,727 of the
// Fossil SCM repository's 67,615 blobs, linking exactly zero of them, on
// every single sync (issue #202).
//
// Canonical fossil does not need such a record because it never sweeps: it
// attempts a crosslink once per blob, as the blob arrives (xfer.c calls
// manifest_crosslink on every accepted file, artifact or not). This table
// gives the sweep the same once-per-blob property. Both facts it records --
// that the blob expanded, and that its bytes are not a parseable artifact --
// are properties of immutable blob content, so the verdict never goes stale;
// a blob that failed to expand, or that parsed but was deferred, is
// deliberately not recorded and is reconsidered by the next sweep.
//
// It is created on demand rather than in db/schema.go so that repositories
// created by canonical fossil -- which sync and clone must both accept --
// gain it on first use instead of being rejected for lacking it. Its contents
// are derived, never authoritative: DROP it and the next sweep re-examines the
// whole repository from the durable blobs, which is what to do if deck.Parse
// ever learns to read an artifact form it used to reject.
func ensureNonArtifactTable(q db.Querier) error {
	if q == nil {
		panic("manifest.ensureNonArtifactTable: q must not be nil")
	}
	if _, err := q.Exec(
		"CREATE TABLE IF NOT EXISTS " + nonArtifactTable + "(rid INTEGER PRIMARY KEY)",
	); err != nil {
		return fmt.Errorf("ensure %s table: %w", nonArtifactTable, err)
	}
	return nil
}

// recordNonArtifacts marks rids as holding no artifact. Called inside the
// batch transaction that examined them, so a rolled-back batch records
// nothing.
func recordNonArtifacts(tx *db.Tx, rids []libfossil.FslID) error {
	if tx == nil {
		panic("manifest.recordNonArtifacts: tx must not be nil")
	}
	for _, rid := range rids {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO "+nonArtifactTable+"(rid) VALUES(?)", int64(rid),
		); err != nil {
			return fmt.Errorf("record non-artifact rid=%d: %w", rid, err)
		}
	}
	return nil
}

// candidate is one not-yet-crosslinked blob discovered by Crosslink's
// candidate query.
type candidate struct {
	rid  libfossil.FslID
	uuid string
}

// deltaChainOrder reorders candidates so that, for any two candidates linked
// by a delta edge within this sweep, the base is visited before the
// dependent -- root first, each descendant exactly one delta application
// after the base it needs, matching Fossil's own rebuild_step shape.
//
// content_deltify stores a blob's older versions as deltas against its
// newer ones, so a candidate's base (delta.srcid) usually has a higher rid
// than the candidate itself: visiting candidates ascending by rid, as the
// query that produced this slice does, visits dependents before their
// bases and forces every chain to materialize in full on its first
// candidate, however far ahead that base is never touched again. Visiting
// bases first means Cache.Expand never has to keep more than the chains
// currently in flight, instead of the whole repository expanded.
//
// This is a topological sort of the candidate set under the "depends on
// its delta base" relation (Kahn's algorithm), computed once per sweep and
// bounded by the candidate count -- it does not walk chain interiors, that
// is Cache.Expand's job on the reordered candidates. Ties -- candidates with
// no unresolved base, ready to visit at the same point -- break by
// ascending rid, preserving the same determinism guarantee the old ORDER BY
// b.rid gave: two syncs that deliver the same blobs in different arrival
// orders still crosslink them in the same relative order.
func deltaChainOrder(q db.Querier, candidates []candidate) ([]candidate, error) {
	if q == nil {
		panic("manifest.deltaChainOrder: q must not be nil")
	}
	if len(candidates) <= 1 {
		return candidates, nil
	}

	inSet := make(map[libfossil.FslID]bool, len(candidates))
	byRid := make(map[libfossil.FslID]candidate, len(candidates))
	for _, c := range candidates {
		inSet[c.rid] = true
		byRid[c.rid] = c
	}

	// children[base] holds every candidate whose delta is stored relative
	// to base, restricted to edges where both ends are candidates in this
	// sweep -- a base outside the candidate set is already expandable on
	// its own and imposes no ordering constraint here.
	children := make(map[libfossil.FslID][]libfossil.FslID)
	hasBase := make(map[libfossil.FslID]bool, len(candidates))

	rows, err := q.Query("SELECT rid, srcid FROM delta")
	if err != nil {
		return nil, fmt.Errorf("manifest.deltaChainOrder query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rid, srcid int64
		if err := rows.Scan(&rid, &srcid); err != nil {
			return nil, fmt.Errorf("manifest.deltaChainOrder scan: %w", err)
		}
		r, s := libfossil.FslID(rid), libfossil.FslID(srcid)
		if inSet[r] && inSet[s] {
			children[s] = append(children[s], r)
			hasBase[r] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("manifest.deltaChainOrder rows: %w", err)
	}

	indegree := make(map[libfossil.FslID]int, len(candidates))
	ready := &ridHeap{}
	heap.Init(ready)
	for _, c := range candidates {
		if hasBase[c.rid] {
			indegree[c.rid] = 1
		} else {
			heap.Push(ready, c.rid)
		}
	}

	ordered := make([]candidate, 0, len(candidates))
	for ready.Len() > 0 {
		rid := heap.Pop(ready).(libfossil.FslID)
		ordered = append(ordered, byRid[rid])
		for _, child := range children[rid] {
			indegree[child]--
			if indegree[child] == 0 {
				heap.Push(ready, child)
			}
		}
	}

	// A candidate's delta chain terminates within maxDeltaChainDepth
	// (content.walkDeltaChain enforces that on every expansion), so it
	// should not cycle back on itself and every candidate should drain
	// from the queue exactly once. The `delta` table is on-disk data --
	// possibly hostile or corrupt, arriving over sync from a remote peer --
	// so a graph that fails to drain is reported as an error rather than
	// treated as a programmer-contract violation.
	if len(ordered) != len(candidates) {
		return nil, fmt.Errorf("manifest.deltaChainOrder: candidate delta graph did not fully drain (%d of %d candidates ordered); delta table may contain a cycle", len(ordered), len(candidates))
	}
	return ordered, nil
}

// ridHeap is a min-heap of rids, used to break deltaChainOrder ties by
// ascending rid.
type ridHeap []libfossil.FslID

func (h ridHeap) Len() int           { return len(h) }
func (h ridHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h ridHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *ridHeap) Push(x any)        { *h = append(*h, x.(libfossil.FslID)) }
func (h *ridHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// Crosslink scans all blobs not yet crosslinked in event/tagxref/forumpost/attachment tables,
// parses them as manifests, and populates cross-reference tables (event/plink/leaf/mlink/tagxref).
// This is the Go equivalent of Fossil's manifest_crosslink.
//
// Candidates are content-expanded in delta-chain order (deltaChainOrder),
// not the ascending-rid order the discovery query returns them in, so a
// whole-repository sweep's working set is bounded by how many delta chains
// are in flight rather than by repository size. Writing event/plink/mlink/
// tagxref-origin rows during that reordered pass is safe because each row
// is a pure function of its own artifact; leaf and tag-propagation state,
// which depend on the whole plink graph rather than any one artifact, are
// deferred to repairLeafTable and repairTagPropagation at the end.
// Crosslink runs the sweep with no cancellation, supplying its own background
// context. It is the historical entry point; callers that can be interrupted
// (a clone bounded by a deadline) should use CrosslinkContext instead.
func Crosslink(r *repo.Repo) (int, error) {
	return CrosslinkContext(context.Background(), r)
}

// CrosslinkContext is a one-shot ReceiveLinker: callers that do not receive
// blobs incrementally get the same candidate ordering, cache, and single repair
// gate as a clone, without retaining a linker beyond this call.
func CrosslinkContext(ctx context.Context, r *repo.Repo) (int, error) {
	linker, err := newReceiveLinker(r, crosslinkCacheBytes)
	if err != nil {
		return 0, err
	}
	return linker.Finalize(ctx)
}

// crosslinkSweepWithState performs only the candidate pass. The caller owns the
// one combined leaf/tag repair gate so receive-time links and sweep links repair
// together, not once each.
func crosslinkSweepWithState(ctx context.Context, r *repo.Repo, st *linkState) (int, error) {
	if r == nil {
		panic("manifest.crosslinkSweepWithState: r must not be nil")
	}
	if st == nil {
		panic("manifest.crosslinkSweepWithState: state must not be nil")
	}
	if err := prepareCrosslinkTables(r); err != nil {
		return 0, err
	}
	candidates, err := collectCrosslinkCandidates(r.DB())
	if err != nil {
		return 0, err
	}
	return linkCandidatesInOrder(ctx, r, candidates, st)
}

func prepareCrosslinkTables(r *repo.Repo) error {
	if r == nil {
		panic("manifest.prepareCrosslinkTables: r must not be nil")
	}
	if err := ensureForumPostTable(r.DB()); err != nil {
		return fmt.Errorf("manifest.Crosslink: %w", err)
	}
	if err := ensureNonArtifactTable(r.DB()); err != nil {
		return fmt.Errorf("manifest.Crosslink: %w", err)
	}
	return nil
}

// repairCrosslinkState runs the single order-sensitive repair gate after every
// derivation that the caller owns has committed.
func repairCrosslinkState(r *repo.Repo, linked int, sweepErr error) (int, error) {
	if linked == 0 {
		return linked, sweepErr
	}
	if err := r.WithTx(func(tx *db.Tx) error {
		if err := repairTagPropagation(tx); err != nil {
			return err
		}
		return repairLeafTable(tx)
	}); err != nil {
		if sweepErr != nil {
			return linked, errors.Join(sweepErr, fmt.Errorf("manifest.Crosslink: %w", err))
		}
		return linked, fmt.Errorf("manifest.Crosslink: %w", err)
	}
	return linked, sweepErr
}

// candidateQuerySQL selects every blob that still owes the sweep an
// examination: it has real content, none of the derived rows a crosslink
// writes, and no recorded verdict that its bytes hold no artifact.
//
// ORDER BY b.rid only seeds deltaChainOrder's tie-break, not the final
// visiting order -- but it must still be deterministic input: deferred
// manifests re-discovered across sweeps need a stable order downstream of it.
// Without it, two syncs delivering the same blobs in different arrival orders
// could produce divergent per-defer slog streams and pending-item processing
// orders, masking determinism bugs in downstream code.
//
// The exclusions are NOT IN rather than correlated NOT EXISTS. Only some of
// the derived tables can answer "is this rid mine?" from an index -- tagxref's
// are on (rid, tagid) and (tagid, mtime), neither covering srcid -- so as a
// correlated subquery tagxref became a full scan per blob: 51,134 x 67,615 row
// visits on the Fossil SCM repository, 99 s of the 164 s a no-op sync took
// before this (issue #202). NOT IN materializes each subquery once and probes
// it, returning the identical rows in 0.08 s. The IS NOT NULL guards are
// required, not cosmetic: NOT IN against a set containing NULL is never true,
// and tagxref.srcid, forumpost.fpid and attachment.attachid are all nullable.
const candidateQuerySQL = `
	SELECT b.rid, b.uuid FROM blob b
	WHERE b.size >= 0
	  AND b.rid NOT IN (SELECT objid FROM event WHERE objid IS NOT NULL)
	  AND b.rid NOT IN (SELECT srcid FROM tagxref WHERE srcid IS NOT NULL)
	  AND b.rid NOT IN (SELECT fpid FROM forumpost WHERE fpid IS NOT NULL)
	  AND b.rid NOT IN (SELECT attachid FROM attachment WHERE attachid IS NOT NULL)
	  AND b.rid NOT IN (SELECT rid FROM ` + nonArtifactTable + `)
	ORDER BY b.rid
`

// collectCrosslinkCandidates returns every not-yet-crosslinked blob, ordered so
// that a delta's base is visited before the delta itself (see deltaChainOrder).
// It is read-only, so it runs on q outside any transaction.
func collectCrosslinkCandidates(q db.Querier) ([]candidate, error) {
	rows, err := q.Query(candidateQuerySQL)
	if err != nil {
		return nil, fmt.Errorf("manifest.Crosslink query: %w", err)
	}
	defer rows.Close()

	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.rid, &c.uuid); err != nil {
			return nil, fmt.Errorf("manifest.Crosslink scan: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("manifest.Crosslink rows: %w", err)
	}

	// Reorder for the content-expansion pass: base before dependent, so
	// Cache.Expand below never has to materialize a chain further ahead
	// than the candidates currently in flight. See deltaChainOrder.
	candidates, err = deltaChainOrder(q, candidates)
	if err != nil {
		return nil, fmt.Errorf("manifest.Crosslink: %w", err)
	}
	return candidates, nil
}

// crosslinkBatchSize is how many candidates one transaction links before it
// commits and the next begins. It trades WAL growth (larger batches let the
// wal-index grow and slow every read) against transaction-setup overhead
// (smaller batches pay a WAL read-transaction setup per batch). A few thousand
// keeps the WAL small enough that read cost stays flat while still amortizing
// setup across the batch.
const crosslinkBatchSize = 2000

// linkState carries the sweep's cross-batch accumulators: one content cache for
// every chain, and the checkinDeferralGuard tracking checkins deferred because
// a referenced blob has not arrived. A ReceiveLinker owns the same state from
// each accepted blob through its final candidate sweep.
type linkState struct {
	cache  *content.Cache
	guard  *checkinDeferralGuard
	linked int
}

func newLinkState(cacheBytes int64) *linkState {
	if cacheBytes <= 0 {
		panic("manifest.newLinkState: cacheBytes must be > 0")
	}
	return &linkState{
		cache: content.NewCache(cacheBytes),
		guard: newCheckinDeferralGuard(),
	}
}

// linkCandidatesInOrder expands and crosslinks every candidate in delta-chain
// order, committing once per crosslinkBatchSize candidates, and returns the
// number of artifacts linked.
func linkCandidatesInOrder(ctx context.Context, r *repo.Repo, candidates []candidate, st *linkState) (int, error) {
	if st == nil {
		panic("manifest.linkCandidatesInOrder: state must not be nil")
	}
	startLinked := st.linked

	for start := 0; start < len(candidates); start += crosslinkBatchSize {
		select {
		case <-ctx.Done():
			return st.linked - startLinked, ctx.Err()
		default:
		}
		end := start + crosslinkBatchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		if err := r.WithTx(func(tx *db.Tx) error {
			return linkBatch(ctx, tx, candidates[start:end], st)
		}); err != nil {
			return st.linked - startLinked, err
		}
	}

	logDeferredCheckinSummary("Crosslink", st.guard, st.linked)

	return st.linked - startLinked, nil
}

// linkBatch links one batch of candidates on tx. ctx is polled once every
// crosslinkCancelCheckStride candidates so a clone deadline can interrupt the
// sweep within a batch, not only at batch boundaries; on cancellation the batch
// rolls back and its links are not counted. Errors from an individual artifact
// abort the whole sweep. A candidate that is not expandable, not a manifest, or
// a deferred checkin is skipped, matching the single-pass behavior this
// batching preserves.
//
// The linked count is accumulated locally and merged into st only once the
// batch completes, so a rolled-back batch (cancelled or errored) contributes
// nothing to the count st reports.
func linkBatch(ctx context.Context, tx *db.Tx, batch []candidate, st *linkState) error {
	batchLinked := 0
	var batchNonArtifact []libfossil.FslID
	for i, c := range batch {
		if i%crosslinkCancelCheckStride == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		data, err := st.cache.Expand(tx, c.rid)
		if err != nil {
			continue // not expandable, skip
		}
		d, err := deck.Parse(data)
		if err != nil {
			// Expanded fine, holds no artifact: ordinary file content. That
			// verdict is a property of immutable bytes, so record it and never
			// expand this blob again (see ensureNonArtifactTable).
			batchNonArtifact = append(batchNonArtifact, c.rid)
			continue
		}
		if !isArtifact(d) {
			// The card grammar was satisfied but the artifact grammar was not.
			// Like a parse failure that is a verdict on immutable bytes, so it
			// is recorded rather than re-derived every sweep. Unlike a deferred
			// check-in below, which is a verdict on what has arrived SO FAR and
			// must stay re-examinable.
			batchNonArtifact = append(batchNonArtifact, c.rid)
			continue
		}
		if d.Type == deck.Checkin && deferCheckin(tx, c, d, st) {
			continue
		}
		handled, linkErr := linkArtifact(tx, c.rid, d, st.cache)
		if linkErr != nil {
			return fmt.Errorf("manifest.Crosslink rid=%d type=%d: %w", c.rid, d.Type, linkErr)
		}
		if !handled {
			continue
		}
		batchLinked++
	}
	if err := recordNonArtifacts(tx, batchNonArtifact); err != nil {
		return err
	}
	st.linked += batchLinked
	return nil
}

// isArtifact reports whether a parsed card deck carries the cards its type
// requires -- that is, whether it is an artifact at all, as opposed to a
// checked-in file whose leading bytes happen to satisfy the card grammar, or a
// malformed artifact Fossil itself refuses.
//
// This is canonical Fossil's required-card table (src/manifest.c
// manifestCardTypes, the zRequired column) applied at the one place it matters
// here: the sweep parses EVERY blob in the repository looking for artifacts, so
// without the gate a file whose first line is a well-formed Z-card links as a
// dateless check-in, and a wiki page written by a client that omitted the
// U-card links as a wiki revision Fossil does not have. Both put rows in event,
// plink, tagxref, and leaf that `fossil rebuild` then removes (issue #193).
//
// deck.Parse deliberately stays permissive -- it is a card-deck parser, and
// this package builds partial decks of its own -- so the artifact-level rule
// lives here, where the question being asked is "is this blob an artifact?".
func isArtifact(d *deck.Deck) bool {
	if d == nil {
		panic("manifest.isArtifact: d must not be nil")
	}
	switch d.Type {
	case deck.Cluster:
		return len(d.M) > 0 // required: M, Z
	case deck.Checkin:
		return !d.D.IsZero() // required: D, Z
	case deck.Control:
		// Required: D, T, U, Z -- and no self-referential T-card. A tag an
		// artifact applies to itself belongs on a check-in; fossil rejects the
		// control artifact outright rather than reinterpreting it
		// (src/manifest.c, "self-referential T-card in control artifact").
		return !d.D.IsZero() && len(d.T) > 0 && d.U != nil && !hasSelfTag(d)
	case deck.Wiki:
		return !d.D.IsZero() && d.L != "" && d.U != nil && d.W != nil
	case deck.Ticket:
		return !d.D.IsZero() && len(d.J) > 0 && d.K != "" && d.U != nil
	case deck.Attachment:
		return !d.D.IsZero() && d.A != nil
	case deck.Event:
		return !d.D.IsZero() && d.E != nil && d.W != nil
	case deck.ForumPost:
		return !d.D.IsZero() && d.U != nil && d.W != nil
	default:
		return !d.D.IsZero()
	}
}

// hasSelfTag reports whether any T-card names the artifact carrying it.
func hasSelfTag(d *deck.Deck) bool {
	for _, tc := range d.T {
		if tc.UUID == "*" {
			return true
		}
	}
	return false
}

// linkArtifact writes the derived rows for one parsed artifact on tx. handled
// is false for artifact types the sweep does not link (so the caller does not
// count them), matching the old switch's `default: continue`.
func linkArtifact(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, cache *content.Cache) (handled bool, err error) {
	switch d.Type {
	case deck.Checkin:
		return true, crosslinkCheckin(tx, rid, d, cache)
	case deck.Wiki:
		return true, crosslinkWiki(tx, rid, d)
	case deck.Ticket:
		return true, crosslinkTicket(tx, rid, d, cache)
	case deck.Event:
		return true, crosslinkEvent(tx, rid, d)
	case deck.Attachment:
		return true, crosslinkAttachment(tx, rid, d)
	case deck.Cluster:
		return true, CrosslinkCluster(tx, rid, d)
	case deck.ForumPost:
		return true, crosslinkForum(tx, rid, d)
	case deck.Control:
		return true, crosslinkControl(tx, rid, d)
	}
	return false, nil
}

// checkinDeferralGuard is the accumulator state behind holding back a Checkin
// manifest whose referenced blobs (F-cards or the B-card baseline) are not
// yet available locally: one availability cache shared across every checkin
// it tests, exact deferred-reference counts for the end-of-run rollup, and
// bounded deterministic samples of deferred rids and missing blob UUIDs.
//
// Both the whole-repository sweep (linkState.guard) and the phantom-fill
// cascade (cascadeLinker.guard) embed one. Sharing the type -- rather than
// each path growing its own copy of this bookkeeping -- is what keeps a
// cascade-deferred checkin falling through to the sweep's own recovery path:
// collectCrosslinkCandidates reselects a rid only when it has NO event row,
// and the cascade has no candidate query or round boundary of its own to
// fall back on. It depends entirely on writing nothing for a deferred rid, so
// a later sweep still sees it as undiscovered. See shouldDefer.
type checkinDeferralGuard struct {
	avail                 *content.AvailabilityCache
	deferredCount         int
	missingReferenceCount int
	deferredRids          []libfossil.FslID
	missingBlobs          map[string]struct{}
}

// newCheckinDeferralGuard returns a guard ready to test checkins for one
// sweep or one cascade run. Not safe for concurrent use; each caller owns one.
func newCheckinDeferralGuard() *checkinDeferralGuard {
	return &checkinDeferralGuard{
		avail:        content.NewAvailabilityCache(),
		deferredRids: make([]libfossil.FslID, 0, logDeferredSampleSize),
		missingBlobs: make(map[string]struct{}, logDeferredSampleSize),
	}
}

// shouldDefer reports whether rid's checkin manifest d must be held back
// because a blob it references (F-cards or the B-card baseline) has not
// arrived locally yet, recording it in g's accumulators when so. source
// labels the caller in the debug log ("Crosslink" or "AfterDephantomize").
//
// The manifest blob remains durable in 'blob'; the caller MUST write nothing
// else -- no event/leaf/plink/mlink/tagxref rows -- for rid when this returns
// true. Skipping every row for it is what keeps a downstream Checkout.Update
// walking the manifest's F-cards via manifest.ListFiles from hitting `blob
// not found` mid-traversal, and what lets a later sweep rediscover and
// complete rid once the missing blob arrives; see the type doc.
//
// Surfaced by agent-infra trial #10 under 16-way concurrent fork+merge: a leaf
// Pulled a multi-blob session in which the merge manifest landed before its
// file blobs, the original crosslink ran with insertCheckinMlinks silently
// skipping missing-blob F-cards, and the next Update on that leaf failed. The
// next sync round that delivers the missing blob also triggers another
// Crosslink sweep (HandleSync runs Crosslink whenever filesRecvd > 0); the
// candidate query selects this rid again because no event row was written, and
// the checkin crosslinks completely.
func (g *checkinDeferralGuard) shouldDefer(tx *db.Tx, source string, rid libfossil.FslID, d *deck.Deck) bool {
	missing := missingCheckinRefs(tx, d, g.avail)
	if len(missing) == 0 {
		return false
	}
	g.recordDeferred(tx, source, rid, missing)
	return true
}

// recordDeferred retains exact diagnostic counts and bounded samples for a
// checkin whose missing references have already been computed by a receive-time
// linker.
func (g *checkinDeferralGuard) recordDeferred(tx *db.Tx, source string, rid libfossil.FslID, missing []string) {
	g.deferredCount++
	g.deferredRids = insertDeferredRIDSample(g.deferredRids, rid)
	for _, uuid := range missing {
		g.missingReferenceCount++
		g.recordMissingBlobSample(uuid)
	}
	var uuid string
	_ = tx.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid)
	slog.Debug("manifest."+source+": deferring checkin",
		"rid", rid,
		"uuid", uuid,
		"missing_count", len(missing),
		"first_missing", missing[0])
}

// recordMissingBlobSample retains only the lexicographically smallest distinct
// missing UUIDs, independent of the order in which checkins are examined.
func (g *checkinDeferralGuard) recordMissingBlobSample(uuid string) {
	if _, exists := g.missingBlobs[uuid]; exists {
		return
	}
	if len(g.missingBlobs) < logDeferredSampleSize {
		g.missingBlobs[uuid] = struct{}{}
		return
	}

	var greatest string
	for sample := range g.missingBlobs {
		if sample > greatest {
			greatest = sample
		}
	}
	if uuid < greatest {
		delete(g.missingBlobs, greatest)
		g.missingBlobs[uuid] = struct{}{}
	}
}

// insertDeferredRIDSample inserts rid into a sorted, distinct, bounded sample
// and discards values greater than the retained smallest rids.
func insertDeferredRIDSample(samples []libfossil.FslID, rid libfossil.FslID) []libfossil.FslID {
	index := sort.Search(len(samples), func(i int) bool { return samples[i] >= rid })
	if index < len(samples) && samples[index] == rid {
		return samples
	}
	if len(samples) < logDeferredSampleSize {
		samples = append(samples, 0)
		copy(samples[index+1:], samples[index:len(samples)-1])
		samples[index] = rid
		return samples
	}
	if index == len(samples) {
		return samples
	}
	copy(samples[index+1:], samples[index:len(samples)-1])
	samples[index] = rid
	return samples
}

// deferCheckin reports whether a checkin must be held back this sweep because
// a blob it references has not arrived locally yet. Thin wrapper over
// checkinDeferralGuard.shouldDefer so linkBatch's call site -- and its
// existing behavior and tests -- do not change.
func deferCheckin(tx *db.Tx, c candidate, d *deck.Deck, st *linkState) bool {
	return st.guard.shouldDefer(tx, "Crosslink", c.rid, d)
}

// logDeferredSampleSize is how many identifiers the deferral rollup prints per
// list. The counts alongside them are what the line is read for -- "this many
// artifacts are waiting on this many blobs" -- and a few examples are enough to
// start chasing a specific stuck artifact. A real clone deferred 180 checkins on
// 417 missing blobs, which rendered in full was a single ~28 KB log line (issue
// #194): unreadable in a terminal and cut at an arbitrary byte by log shippers.
const logDeferredSampleSize = 8

// logDeferredCheckinSummary emits the production guard rollup. Observation
// counts and bounded deterministic samples are intentionally named separately:
// samples never imply an omitted-count relationship.
func logDeferredCheckinSummary(source string, guard *checkinDeferralGuard, linked int) {
	if guard == nil || guard.deferredCount == 0 {
		return
	}
	rids := make([]string, len(guard.deferredRids))
	for i, rid := range guard.deferredRids {
		rids[i] = strconv.FormatInt(int64(rid), 10)
	}
	slog.Info("manifest."+source+": deferred checkins awaiting missing blobs",
		"defer_attempt_count", guard.deferredCount,
		"missing_reference_observation_count", guard.missingReferenceCount,
		"deferred_rid_sample", strings.Join(rids, " "),
		"missing_uuid_sample", strings.Join(smallestMissingBlobSample(guard.missingBlobs), " "),
		"linked", linked)
}

// logDeferredCheckins is retained for direct-input callers and tests. It
// derives the same deterministic smallest samples as a guard while keeping
// exact len-based counts for its unbounded input collections.
func logDeferredCheckins(source string, deferredRids []libfossil.FslID, missingBlobs map[string]struct{}, linked int) {
	if len(deferredRids) == 0 {
		return
	}
	logDeferredCheckinLegacyRollup(
		source,
		len(deferredRids),
		smallestDeferredRIDSample(deferredRids),
		len(missingBlobs),
		smallestMissingBlobSample(missingBlobs),
		linked,
	)
}

func logDeferredCheckinLegacyRollup(source string, deferredCount int, deferredRids []libfossil.FslID, missingReferenceCount int, missingBlobs []string, linked int) {
	rids := make([]string, len(deferredRids))
	for i, rid := range deferredRids {
		rids[i] = strconv.FormatInt(int64(rid), 10)
	}
	slog.Info("manifest."+source+": deferred checkins awaiting missing blobs",
		"deferred", deferredCount,
		"linked", linked,
		"deferred_rids", sampledList(rids, deferredCount),
		"missing_blob_count", missingReferenceCount,
		"missing_blobs", sampledList(missingBlobs, missingReferenceCount))
}

func smallestDeferredRIDSample(rids []libfossil.FslID) []libfossil.FslID {
	sample := make([]libfossil.FslID, 0, logDeferredSampleSize)
	for _, rid := range rids {
		sample = insertDeferredRIDSample(sample, rid)
	}
	return sample
}

func smallestMissingBlobSample(blobs map[string]struct{}) []string {
	sample := make([]string, 0, logDeferredSampleSize)
	for uuid := range blobs {
		sample = insertMissingBlobSample(sample, uuid)
	}
	return sample
}

func insertMissingBlobSample(samples []string, uuid string) []string {
	index := sort.SearchStrings(samples, uuid)
	if index < len(samples) && samples[index] == uuid {
		return samples
	}
	if len(samples) < logDeferredSampleSize {
		samples = append(samples, "")
		copy(samples[index+1:], samples[index:len(samples)-1])
		samples[index] = uuid
		return samples
	}
	if index == len(samples) {
		return samples
	}
	copy(samples[index+1:], samples[index:len(samples)-1])
	samples[index] = uuid
	return samples
}

// sampledList renders a bounded sample while preserving its exact total count.
func sampledList(items []string, total int) string {
	if total <= len(items) {
		return strings.Join(items, " ")
	}
	return fmt.Sprintf("%s ...and %d more",
		strings.Join(items, " "), total-len(items))
}

// repairTagPropagation re-runs tag propagation from every primary parent,
// once, now that the whole sweep's plink edges are in place.
//
// Canonical fossil seeds propagation the same way: having inserted a new
// check-in's plink edges it calls tag_propagate_all on the *primary parent*
// rather than on whichever artifact originally declared the tag
// (src/manifest.c:2300-2302 and 2467-2469). The parent is the right seed
// because tag_propagate's per-child test is strict:
//
//	coalesce(srcid=0 AND tagxref.mtime<:mtime, 1) AS doit
//
// A descendant that already carries the tag holds it at exactly the origin's
// mtime, so `mtime < mtime` is false, doit is 0, and the walk neither retags
// that child nor queues it for further descent. Seeding from an origin
// therefore stops dead at the first descendant that already has the tag,
// which silently strands any check-in linked after that descendant was --
// deferred behind a missing blob, or arriving in a later clone round. Seeding
// from each parent instead only ever tests that parent's own children, so a
// newly linked check-in is reached however late it arrives (issue #198).
//
// Running it once per parent, in any order, converges: tag.propagate walks the
// plink table live at call time rather than a snapshot, and cascades through
// descendants that hold no row of their own, so a parent visited before its
// ancestors are tagged contributes nothing then but is reached by the ancestor's
// own cascade. This replaces propagating once per checkin at the moment each
// checkin was linked, which depended on ancestors being crosslinked before
// their descendants -- true for an ascending-rid sweep, false for delta-chain
// order (see applyInlineTags and addFWTPlink).
func repairTagPropagation(q db.Querier) error {
	if q == nil {
		panic("manifest.repairTagPropagation: q must not be nil")
	}

	rows, err := q.Query("SELECT DISTINCT pid FROM plink WHERE isprim = 1")
	if err != nil {
		return fmt.Errorf("repairTagPropagation query: %w", err)
	}
	var parents []libfossil.FslID
	for rows.Next() {
		var pid int64
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return fmt.Errorf("repairTagPropagation scan: %w", err)
		}
		parents = append(parents, libfossil.FslID(pid))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("repairTagPropagation rows: %w", err)
	}

	for _, pid := range parents {
		if err := tag.PropagateAll(q, pid); err != nil {
			return fmt.Errorf("repairTagPropagation propagate rid=%d: %w", pid, err)
		}
	}
	return nil
}

// missingCheckinRefs returns the list of UUIDs referenced by a Checkin
// manifest whose blobs are not yet present locally. References checked:
//   - B-card: the baseline manifest UUID for delta manifests. Without
//     the baseline, ListFiles cannot resolve the effective F-card set.
//   - F-cards: every (non-deleted) file UUID. These are the targets
//     Checkout.Update.expandUUID will need.
//
// Empty result means crosslink is safe to run; non-empty means defer
// to a later sweep that will discover the manifest again (no event row
// was written, so the candidate query re-selects this rid).
//
// Divergence from fossil-scm/c: fossil's reference uses an `rcvfrom`
// table + deferred-flush at content arrival; the Go port reuses the
// existing whole-repo sweep semantics by checking presence at sweep
// time. The candidate query naturally re-discovers deferred manifests
// because we do not write any event/leaf/plink/mlink/tagxref rows for
// them.
func missingCheckinRefs(tx *db.Tx, d *deck.Deck, avail *content.AvailabilityCache) []string {
	if tx == nil {
		panic("manifest.missingCheckinRefs: tx must not be nil")
	}
	if d == nil {
		panic("manifest.missingCheckinRefs: d must not be nil")
	}
	var missing []string
	seen := make(map[string]struct{})
	check := func(uuid string) {
		if uuid == "" {
			return
		}
		if _, dup := seen[uuid]; dup {
			return
		}
		seen[uuid] = struct{}{}
		if _, ok := avail.ByUUID(tx, uuid); !ok {
			missing = append(missing, uuid)
		}
	}
	check(d.B)
	for _, f := range d.F {
		check(f.UUID) // skipped if "" (deleted file in delta manifest)
	}
	return missing
}

// crosslinkCheckin links one check-in manifest. cache is the caller's shared
// content cache -- the whole-sweep one, or the one cascadeLinker runs for a
// phantom-fill cascade; insertCheckinMlinks uses it to expand the parent
// manifest at most once per parent across the run. A nil cache is legal and
// expands every parent from scratch, but no caller passes one.
func crosslinkCheckin(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, cache *content.Cache) error {
	if tx == nil {
		panic("crosslinkCheckin: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkCheckin: rid must be positive")
	}

	if err := crosslinkCheckinTables(tx, rid, d, cache); err != nil {
		return err
	}
	return applyInlineTags(tx, rid, d)
}

// crosslinkCheckinTables populates event/plink/mlink/cherrypick on tx. leaf is
// deliberately not touched here -- see repairLeafTable. tx is the whole
// sweep's single transaction (see CrosslinkContext), so these writes commit
// atomically with every other candidate's, not one transaction per checkin.
func crosslinkCheckinTables(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, cache *content.Cache) error {
	// event
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO event(type, mtime, objid, user, comment) VALUES('ci', ?, ?, ?, ?)",
		libfossil.TimeToJulian(d.D), rid, d.U, d.C,
	); err != nil {
		return fmt.Errorf("event: %w", err)
	}

	// Resolve baseid for plink if B-card present
	var baseid any = nil
	if d.B != "" {
		var baseRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", d.B).Scan(&baseRid); err == nil {
			baseid = baseRid
		}
	}

	if err := insertCheckinPlinks(tx, rid, d, baseid); err != nil {
		return err
	}
	if err := insertCheckinMlinks(tx, cache, rid, d); err != nil {
		return err
	}
	return insertCherrypicks(tx, rid, d)
}

// insertCheckinPlinks inserts plink rows for each parent (P-card).
func insertCheckinPlinks(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, baseid any) error {
	for i, parentUUID := range d.P {
		parentRid, err := ridOrPhantom(tx, parentUUID)
		if err != nil {
			return fmt.Errorf("plink parent %s: %w", parentUUID, err)
		}
		if parentRid <= 0 {
			continue
		}
		isPrim := 0
		if i == 0 {
			isPrim = 1
		}
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO plink(pid, cid, isprim, mtime, baseid) VALUES(?, ?, ?, ?, ?)",
			parentRid, rid, isPrim, libfossil.TimeToJulian(d.D), baseid,
		); err != nil {
			return fmt.Errorf("plink: %w", err)
		}
	}
	return nil
}

// ridOrPhantom resolves an artifact hash to its blob rid, reserving a phantom
// blob for a hash that has not arrived. It is canonical Fossil's
// uuid_to_rid(zUuid, 1).
//
// The ancestry a check-in declares is a property of the check-in, not of what
// happens to be in the repository when it is crosslinked, so the plink edge has
// to exist either way. Skipping the edge instead lost it permanently: nothing
// revisits an already-linked artifact, so a parent that arrived in a later
// clone round -- or a shunned parent that never arrives -- left a hole in the
// ancestry graph, and with it a hole in leaf (issue #193). Reserving the rid
// now also means the edge already points at the right row when the content does
// arrive and dephantomizes it in place.
func ridOrPhantom(q db.Querier, uuid string) (int64, error) {
	if q == nil {
		panic("manifest.ridOrPhantom: q must not be nil")
	}
	if uuid == "" {
		return 0, nil
	}
	if rid, ok := blob.Exists(q, uuid); ok {
		return int64(rid), nil
	}
	rid, err := blob.StorePhantom(q, uuid)
	if err != nil {
		return 0, err
	}
	return int64(rid), nil
}

// expandManifestBytes returns a blob's fully-expanded content, using cache when
// one is supplied (the whole-repo sweep passes its shared cache so each parent
// manifest's delta chain is walked at most once across every child that
// references it) and falling back to a direct expansion otherwise.
func expandManifestBytes(q db.Querier, cache *content.Cache, rid libfossil.FslID) ([]byte, error) {
	if q == nil {
		panic("manifest.expandManifestBytes: q must not be nil")
	}
	if cache != nil {
		return cache.Expand(q, rid)
	}
	return content.Expand(q, rid)
}

// resolveParentMids resolves a checkin manifest's P-card UUIDs to blob
// rids: the first is the primary parent, any remaining are merge parents.
// A parent UUID whose blob has not arrived locally is skipped, mirroring
// insertCheckinPlinks' existing tolerance for missing parent blobs.
func resolveParentMids(tx *db.Tx, d *deck.Deck) (primaryParentMid libfossil.FslID, mergeParentMids []libfossil.FslID) {
	if tx == nil {
		panic("manifest.resolveParentMids: tx must not be nil")
	}
	if d == nil {
		panic("manifest.resolveParentMids: d must not be nil")
	}
	if len(d.P) > maxMlinkMergeParents+1 {
		panic("manifest.resolveParentMids: d.P exceeds bound")
	}
	for i, parentUUID := range d.P {
		var parentRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", parentUUID).Scan(&parentRid); err != nil {
			continue // parent blob missing, skip
		}
		if i == 0 {
			primaryParentMid = libfossil.FslID(parentRid)
		} else {
			mergeParentMids = append(mergeParentMids, libfossil.FslID(parentRid))
		}
	}
	return primaryParentMid, mergeParentMids
}

// insertCherrypicks inserts cherrypick rows for Q-cards (cherrypick/backout).
func insertCherrypicks(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	for _, cp := range d.Q {
		target := cp.Target
		isExclude := 0
		if cp.IsBackout {
			isExclude = 1
		}
		var parentRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", target).Scan(&parentRid); err != nil {
			continue // target blob missing, skip
		}
		if _, err := tx.Exec(
			"REPLACE INTO cherrypick(parentid, childid, isExclude) VALUES(?, ?, ?)",
			parentRid, rid, isExclude,
		); err != nil {
			return fmt.Errorf("cherrypick: %w", err)
		}
	}
	return nil
}

// applyInlineTags records a check-in's T-cards as tagxref rows.
//
// A T-card's UUID names the artifact the tag lands on. "*" means the check-in
// carrying the card, which is how a check-in declares its own branch, closes
// itself, or sets its own colour. Any other UUID targets a DIFFERENT artifact:
// canonical Fossil applies those from a check-in exactly as it does from a
// control artifact (src/manifest.c, the CFTYPE_CONTROL || CFTYPE_MANIFEST ||
// CFTYPE_EVENT block calls tag_insert for every T-card, resolving the target
// with uuid_to_rid). Dropping them cost 612 `closed` rows on the Fossil
// self-hosting repository -- every `merge --integrate` records the branch it
// closed as a T-card on the merge check-in, not as a separate control artifact
// (issue #193).
//
// It used to also re-run tag.PropagateAll from the primary parent here, to
// pull down whatever the parent's ancestry carried onto this checkin the
// moment it was linked. That only reached children already present in
// plink, which made it depend on ancestors being crosslinked before their
// descendants -- true for an ascending-rid sweep, false for delta-chain
// order. repairTagPropagation now does this once, for every self-declared
// tag origin, after the whole sweep's plink edges are in place; see there
// for why running it once per origin in any order still converges.
func applyInlineTags(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	mtime := libfossil.TimeToJulian(d.D)
	for _, tc := range d.T {
		targetRID := rid
		if tc.UUID != "*" {
			// Canonical resolves the target with uuid_to_rid(...,1), reserving a
			// phantom for an artifact that has not arrived, so the tag lands on
			// the row the content will eventually fill. See ridOrPhantom.
			target, err := ridOrPhantom(tx, tc.UUID)
			if err != nil {
				return fmt.Errorf("tag target %s: %w", tc.UUID, err)
			}
			if target <= 0 {
				continue
			}
			targetRID = libfossil.FslID(target)
		}
		var tagType int
		switch tc.Type {
		case deck.TagPropagating:
			tagType = tag.TagPropagating
		case deck.TagSingleton:
			tagType = tag.TagSingleton
		case deck.TagCancel:
			tagType = tag.TagCancel
		default:
			continue
		}

		if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
			TargetRID: targetRID,
			SrcRID:    rid, // the check-in carrying the card is the source
			TagName:   tc.Name,
			TagType:   tagType,
			Value:     tc.Value,
			MTime:     mtime,
		}); err != nil {
			return fmt.Errorf("inline tag %q: %w", tc.Name, err)
		}
	}

	return nil
}

func crosslinkControl(tx *db.Tx, srcRID libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("crosslinkControl: tx must not be nil")
	}
	if srcRID <= 0 {
		panic("crosslinkControl: rid must be positive")
	}

	mtime := libfossil.TimeToJulian(d.D)
	for _, tc := range d.T {
		if tc.UUID == "*" {
			continue // self-referencing — handled in crosslinkCheckin
		}
		targetRID, err := ridOrPhantom(tx, tc.UUID)
		if err != nil {
			return fmt.Errorf("tag target %s: %w", tc.UUID, err)
		}
		if targetRID <= 0 {
			continue
		}
		var tagType int
		switch tc.Type {
		case deck.TagPropagating:
			tagType = tag.TagPropagating
		case deck.TagSingleton:
			tagType = tag.TagSingleton
		case deck.TagCancel:
			tagType = tag.TagCancel
		default:
			continue
		}
		if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
			TargetRID: libfossil.FslID(targetRID),
			SrcRID:    srcRID,
			TagName:   tc.Name,
			TagType:   tagType,
			Value:     tc.Value,
			MTime:     mtime,
		}); err != nil {
			return fmt.Errorf("apply tag %q to rid=%d: %w", tc.Name, targetRID, err)
		}
	}

	// Generate event row with type='g' and descriptive comment.
	comment := buildControlComment(d)
	if _, err := tx.Exec(
		"REPLACE INTO event(type, mtime, objid, user, comment) VALUES('g', ?, ?, ?, ?)",
		mtime, srcRID, d.U, comment,
	); err != nil {
		return fmt.Errorf("control event: %w", err)
	}

	return nil
}

// buildControlComment generates a human-readable comment from a control artifact's T-cards.
func buildControlComment(d *deck.Deck) string {
	var comment string
	for _, tc := range d.T {
		if tc.UUID == "*" {
			continue
		}
		prefix := string(tc.Type)
		name := tc.Name
		val := tc.Value
		switch {
		case prefix == "*" && name == "branch":
			comment += fmt.Sprintf(" Move to branch %s.", val)
		case prefix == "*" && name == "bgcolor":
			comment += fmt.Sprintf(" Change branch background color to %q.", val)
		case prefix == "+" && name == "bgcolor":
			comment += fmt.Sprintf(" Change background color to %q.", val)
		case prefix == "-" && name == "bgcolor":
			comment += " Cancel background color."
		case prefix == "+" && name == "comment":
			comment += " Edit check-in comment."
		case prefix == "+" && name == "user":
			comment += fmt.Sprintf(" Change user to %q.", val)
		default:
			switch prefix {
			case "-":
				comment += fmt.Sprintf(" Cancel %q.", name)
			case "+":
				comment += fmt.Sprintf(" Add %q.", name)
			case "*":
				comment += fmt.Sprintf(" Add propagating %q.", name)
			}
		}
	}
	if comment == "" {
		comment = " "
	}
	return comment
}

// addFWTPlink inserts plink rows for wiki/forum/technote/ticket P-cards.
// Shared helper for artifact types that use P-cards (parents) but not the
// full checkin flow.
//
// It used to also call tag.PropagateAll from the primary parent here, for
// the same reason applyInlineTags did (see its comment): repairTagPropagation
// now owns that, once, after the sweep.
func addFWTPlink(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("manifest.addFWTPlink: tx must not be nil")
	}
	if rid <= 0 {
		panic("manifest.addFWTPlink: rid must be positive")
	}

	mtime := libfossil.TimeToJulian(d.D)

	for i, parentUUID := range d.P {
		parentRid, err := ridOrPhantom(tx, parentUUID)
		if err != nil {
			return fmt.Errorf("addFWTPlink parent %s: %w", parentUUID, err)
		}
		if parentRid <= 0 {
			continue
		}
		isPrim := 0
		if i == 0 {
			isPrim = 1
		}
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO plink(pid, cid, isprim, mtime) VALUES(?, ?, ?, ?)",
			parentRid, rid, isPrim, mtime,
		); err != nil {
			return fmt.Errorf("addFWTPlink: %w", err)
		}
	}

	return nil
}

// wikiContentLen is the length Fossil records in a wiki-<title> or
// event-<id> tag: the W-card body with its LEADING whitespace skipped
// (src/manifest.c, `while( fossil_isspace(p->zWiki[0]) ) p->zWiki++`). Counting
// the raw card body instead put a value two or three bytes too large on every
// revision of the wiki pages that begin with a blank line, which is a tagxref
// difference even though the page content is identical (issue #193).
func wikiContentLen(w []byte) int {
	i := 0
	for i < len(w) {
		switch w[i] {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			i++
		default:
			return len(w) - i
		}
	}
	return 0
}

func crosslinkWiki(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("crosslinkWiki: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkWiki: rid must be positive")
	}

	if err := addFWTPlink(tx, rid, d); err != nil {
		return fmt.Errorf("wiki plink: %w", err)
	}

	title := d.L
	if title == "" {
		return fmt.Errorf("wiki manifest missing title (L-card)")
	}

	// Apply wiki-<title> tag with value = content length
	wikiLen := fmt.Sprintf("%d", wikiContentLen(d.W))
	if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
		TargetRID: rid,
		SrcRID:    rid,
		TagName:   fmt.Sprintf("wiki-%s", title),
		TagType:   tag.TagSingleton,
		Value:     wikiLen,
		MTime:     libfossil.TimeToJulian(d.D),
	}); err != nil {
		return fmt.Errorf("wiki tag: %w", err)
	}

	// Insert event row with prefix: '+' = new, ':' = edit, '-' = delete
	var prefix byte
	if len(d.W) == 0 {
		prefix = '-' // deletion
	} else if len(d.P) == 0 {
		prefix = '+' // new page
	} else {
		prefix = ':' // edit
	}
	comment := fmt.Sprintf("%c%s", prefix, title)

	if _, err := tx.Exec(
		"REPLACE INTO event(type, mtime, objid, user, comment) VALUES('w', ?, ?, ?, ?)",
		libfossil.TimeToJulian(d.D), rid, d.U, comment,
	); err != nil {
		return fmt.Errorf("wiki event: %w", err)
	}

	return nil
}

// crosslinkTicket links one ticket-change artifact: it applies the tkt-<uuid>
// tag that marks the artifact as belonging to its ticket, then rebuilds every
// derived row that ticket owns from the full tag set (see ticketRebuildEntry).
//
// The rebuild runs here, in the caller's transaction, rather than after the
// sweep. The tag is what collectCrosslinkCandidates reads as "this blob is
// already linked", so anything written after the tag but outside its
// transaction can be skipped without trace -- which is exactly what issue #184
// was. Both writes now commit together or roll back together.
func crosslinkTicket(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, cache *content.Cache) error {
	if tx == nil {
		panic("crosslinkTicket: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkTicket: rid must be positive")
	}

	ticketUUID := d.K
	if ticketUUID == "" {
		return fmt.Errorf("ticket manifest missing UUID (K-card)")
	}
	if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
		TargetRID: rid,
		SrcRID:    rid,
		TagName:   fmt.Sprintf("tkt-%s", ticketUUID),
		TagType:   tag.TagSingleton,
		MTime:     libfossil.TimeToJulian(d.D),
	}); err != nil {
		return fmt.Errorf("ticket tag: %w", err)
	}
	if err := ticketRebuildEntry(tx, cache, ticketUUID); err != nil {
		return fmt.Errorf("ticket rebuild: %w", err)
	}
	if err := updateAttachmentComments(tx, ticketUUID, 't'); err != nil {
		return fmt.Errorf("ticket attachment comments: %w", err)
	}
	return nil
}

func crosslinkEvent(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("crosslinkEvent: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkEvent: rid must be positive")
	}

	if d.E == nil {
		return fmt.Errorf("event manifest missing E-card")
	}
	if err := addFWTPlink(tx, rid, d); err != nil {
		return fmt.Errorf("event plink: %w", err)
	}
	// A technote's T-cards are applied exactly as a check-in's or a control
	// artifact's are -- canonical Fossil runs all three through one block
	// (src/manifest.c, CFTYPE_CONTROL || CFTYPE_MANIFEST || CFTYPE_EVENT). This
	// has to happen before the event row is written below, because that row
	// reads the technote's own bgcolor tag back out of tagxref.
	if err := applyInlineTags(tx, rid, d); err != nil {
		return fmt.Errorf("event tags: %w", err)
	}

	eventID := d.E.UUID
	tagName := fmt.Sprintf("event-%s", eventID)
	mtime := libfossil.TimeToJulian(d.D)
	if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
		TargetRID: rid,
		SrcRID:    rid,
		TagName:   tagName,
		TagType:   tag.TagSingleton,
		Value:     fmt.Sprintf("%d", wikiContentLen(d.W)),
		MTime:     mtime,
	}); err != nil {
		return fmt.Errorf("event tag: %w", err)
	}

	var tagid int64
	if err := tx.QueryRow("SELECT tagid FROM tag WHERE tagname=?", tagName).Scan(&tagid); err != nil {
		return fmt.Errorf("event tagid: %w", err)
	}

	var subsequent int64
	tx.QueryRow("SELECT rid FROM tagxref WHERE tagid=? AND mtime>=? AND rid!=? ORDER BY mtime LIMIT 1",
		tagid, mtime, rid).Scan(&subsequent)

	// Fossil deletes stale event rows when a newer version of this tech note exists
	// but no subsequent version has been crosslinked yet. This ensures only the latest
	// version's event row survives, preventing duplicate timeline entries.
	//
	// This stays correct however the sweep orders candidates, delta-chain
	// order included: ApplyTag above always records this revision's own
	// tagxref row before the check runs, so "subsequent" accumulates every
	// revision seen so far regardless of visiting order. Whichever revision
	// is the true global-mtime-max always finds subsequent==0 when its own
	// turn comes -- nothing else can have a mtime >= a maximum -- and does
	// the delete+insert then, even if some earlier-visited, lower-mtime
	// revision inserted a since-stale event row first.
	if len(d.P) > 0 && subsequent == 0 {
		tx.Exec("DELETE FROM event WHERE type='e' AND tagid=? AND objid IN (SELECT rid FROM tagxref WHERE tagid=?)", tagid, tagid)
	}
	if subsequent == 0 {
		var bgcolor any
		var bgStr string
		if tx.QueryRow("SELECT value FROM tagxref JOIN tag USING(tagid) WHERE tagname='bgcolor' AND rid=?", rid).Scan(&bgStr) == nil {
			bgcolor = bgStr
		}
		if _, err := tx.Exec(
			"REPLACE INTO event(type, mtime, objid, tagid, user, comment, bgcolor) VALUES('e', ?, ?, ?, ?, ?, ?)",
			libfossil.TimeToJulian(d.E.Date), rid, tagid, d.U, d.C, bgcolor,
		); err != nil {
			return fmt.Errorf("event insert: %w", err)
		}
	}
	if err := updateAttachmentComments(tx, eventID, 'e'); err != nil {
		return fmt.Errorf("event attachment comments: %w", err)
	}
	return nil
}

func crosslinkAttachment(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("crosslinkAttachment: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkAttachment: rid must be positive")
	}

	if d.A == nil {
		return fmt.Errorf("attachment manifest missing A-card")
	}
	mtime := libfossil.TimeToJulian(d.D)
	src, target, filename := d.A.Source, d.A.Target, d.A.Filename

	if _, err := tx.Exec(
		"INSERT INTO attachment(attachid, mtime, src, target, filename, comment, user) VALUES(?, ?, ?, ?, ?, ?, ?)",
		rid, mtime, src, target, filename, d.C, d.U,
	); err != nil {
		return fmt.Errorf("attachment insert: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE attachment SET isLatest = (mtime = (SELECT max(mtime) FROM attachment WHERE target=? AND filename=?)) WHERE target=? AND filename=?`,
		target, filename, target, filename,
	); err != nil {
		return fmt.Errorf("attachment isLatest: %w", err)
	}

	// Fossil defaults to wiki when target is not a hash (page name = wiki target).
	// Only hash-shaped targets can refer to tickets or tech notes.
	attachToType := byte('w')
	if isHash(target) {
		var dummy int
		if tx.QueryRow("SELECT 1 FROM tag WHERE tagname=?", "tkt-"+target).Scan(&dummy) == nil {
			attachToType = 't'
		} else if tx.QueryRow("SELECT 1 FROM tag WHERE tagname=?", "event-"+target).Scan(&dummy) == nil {
			attachToType = 'e'
		}
	}

	typeName := attachTargetTypeName[attachToType]
	var evComment string
	if src != "" {
		evComment = fmt.Sprintf("Add attachment %s to %s %s", filename, typeName, target)
	} else {
		evComment = fmt.Sprintf("Delete attachment %q from %s %s", filename, typeName, target)
	}
	if _, err := tx.Exec("REPLACE INTO event(type, mtime, objid, user, comment) VALUES(?, ?, ?, ?, ?)",
		string(attachToType), mtime, rid, d.U, evComment); err != nil {
		return fmt.Errorf("attachment event: %w", err)
	}
	return nil
}

func isHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func updateAttachmentComments(tx *db.Tx, targetID string, targetType byte) error {
	if tx == nil {
		panic("updateAttachmentComments: tx must not be nil")
	}
	if targetID == "" {
		panic("updateAttachmentComments: targetID must not be empty")
	}

	// Read the whole attachment set before issuing any UPDATE. A single
	// transaction holds one connection, and iterating this SELECT cursor while
	// running the UPDATEs below would use that connection for two statements at
	// once. Materializing first keeps read and writes strictly sequential.
	type attachRow struct {
		attachid    int64
		src, target string
		filename    string
	}
	rows, err := tx.Query("SELECT attachid, src, target, filename FROM attachment WHERE target=?", targetID)
	if err != nil {
		return fmt.Errorf("updateAttachmentComments query: %w", err)
	}
	var attachments []attachRow
	for rows.Next() {
		var a attachRow
		if rows.Scan(&a.attachid, &a.src, &a.target, &a.filename) != nil {
			continue
		}
		attachments = append(attachments, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("updateAttachmentComments rows: %w", err)
	}
	rows.Close()

	typeName := attachTargetTypeName[targetType]
	for _, a := range attachments {
		var comment string
		if a.src != "" {
			comment = fmt.Sprintf("Add attachment %s to %s %s", a.filename, typeName, a.target)
		} else {
			comment = fmt.Sprintf("Delete attachment %q from %s %s", a.filename, typeName, a.target)
		}
		if _, err := tx.Exec("UPDATE event SET comment=?, type=? WHERE objid=?", comment, string(targetType), a.attachid); err != nil {
			return fmt.Errorf("updateAttachmentComments event update: %w", err)
		}
	}
	return nil
}

func crosslinkForum(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("crosslinkForum: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkForum: rid must be positive")
	}

	if err := addFWTPlink(tx, rid, d); err != nil {
		return fmt.Errorf("forum plink: %w", err)
	}

	// Resolve thread references.
	froot, fprev, firt, err := resolveForumRefs(tx, rid, d)
	if err != nil {
		return fmt.Errorf("forum references: %w", err)
	}

	// Insert forumpost
	if _, err := tx.Exec(
		"REPLACE INTO forumpost(fpid, froot, fprev, firt, fmtime) VALUES(?, ?, nullif(?, 0), nullif(?, 0), ?)",
		rid, froot, fprev, firt, libfossil.TimeToJulian(d.D),
	); err != nil {
		return fmt.Errorf("forumpost insert: %w", err)
	}

	mtime := libfossil.TimeToJulian(d.D)

	if firt == 0 {
		return crosslinkForumStarter(tx, rid, d, froot, fprev, mtime)
	}
	return crosslinkForumReply(tx, rid, d, froot, fprev, mtime)
}

// resolveForumRefs resolves the thread root, previous, and in-reply-to rids
// from deck cards, reserving stable phantom rids for references not yet stored.
func resolveForumRefs(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) (froot, fprev, firt libfossil.FslID, err error) {
	if d.G == "" {
		froot = rid // self is thread root only when no G-card declares one.
	} else {
		resolved, err := ridOrPhantom(tx, d.G)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("forum root %q: %w", d.G, err)
		}
		froot = libfossil.FslID(resolved)
	}
	if len(d.P) > 0 && d.P[0] != "" {
		resolved, err := ridOrPhantom(tx, d.P[0])
		if err != nil {
			return 0, 0, 0, fmt.Errorf("forum previous %q: %w", d.P[0], err)
		}
		fprev = libfossil.FslID(resolved)
	}
	if d.I != "" {
		resolved, err := ridOrPhantom(tx, d.I)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("forum in-reply-to %q: %w", d.I, err)
		}
		firt = libfossil.FslID(resolved)
	}
	return froot, fprev, firt, nil
}

// crosslinkForumStarter inserts the event row for a thread-starting forum post.
func crosslinkForumStarter(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, froot, fprev libfossil.FslID, mtime float64) error {
	title := d.H
	if title == "" {
		title = "(Deleted)"
	}
	fType := "Post"
	if fprev != 0 {
		fType = "Edit"
	}
	if _, err := tx.Exec(
		"REPLACE INTO event(type, mtime, objid, user, comment) VALUES('f', ?, ?, ?, ?)",
		mtime, rid, d.U, fmt.Sprintf("%s: %s", fType, title),
	); err != nil {
		return fmt.Errorf("forum event: %w", err)
	}
	// Update thread title if most recent. Confluent the same way the
	// tech-note event replacement above is: the REPLACE into forumpost just
	// above always records this post's own fmtime first, so hasNewer
	// accumulates over whatever thread members have been visited so far
	// regardless of order, and the true latest post always finds hasNewer==0
	// on its own turn, overwriting anything an earlier-visited, older post
	// wrote first.
	var hasNewer int
	tx.QueryRow("SELECT count(*) FROM forumpost WHERE froot=? AND firt=0 AND fpid!=? AND fmtime>?",
		froot, rid, mtime).Scan(&hasNewer)
	if hasNewer == 0 {
		tx.Exec(
			"UPDATE event SET comment=substr(comment,1,instr(comment,':')) || ' ' || ? WHERE objid IN (SELECT fpid FROM forumpost WHERE froot=?)",
			title, froot)
	}
	return nil
}

// crosslinkForumReply inserts the event row for a forum reply.
func crosslinkForumReply(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, froot, fprev libfossil.FslID, mtime float64) error {
	var rootTitle string
	if tx.QueryRow("SELECT substr(comment, instr(comment,':')+2) FROM event WHERE objid=?", froot).Scan(&rootTitle) != nil {
		rootTitle = "Unknown"
	}
	fType := "Reply"
	if len(d.W) == 0 {
		fType = "Delete reply"
	} else if fprev != 0 {
		fType = "Edit reply"
	}
	if _, err := tx.Exec(
		"REPLACE INTO event(type, mtime, objid, user, comment) VALUES('f', ?, ?, ?, ?)",
		mtime, rid, d.U, fmt.Sprintf("%s: %s", fType, rootTitle),
	); err != nil {
		return fmt.Errorf("forum reply event: %w", err)
	}
	return nil
}

// CrosslinkCluster processes a cluster artifact: applies the cluster singleton
// tag (tagid=7), removes clustered blobs from unclustered, and creates phantoms
// for any referenced UUIDs not yet in the blob table.
func CrosslinkCluster(q db.Querier, rid libfossil.FslID, d *deck.Deck) error {
	if q == nil {
		panic("manifest.CrosslinkCluster: q must not be nil")
	}
	if rid <= 0 {
		panic("manifest.CrosslinkCluster: rid must be > 0")
	}
	if d == nil {
		panic("manifest.CrosslinkCluster: d must not be nil")
	}

	// Apply cluster singleton tag (tagid=7, tagtype=1).
	if _, err := q.Exec(
		"INSERT OR REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) VALUES(7, 1, ?, ?, NULL, 0, ?)",
		rid, rid, rid,
	); err != nil {
		return fmt.Errorf("manifest.CrosslinkCluster tag: %w", err)
	}

	// Process each M-card UUID.
	for _, uuid := range d.M {
		memberRID, exists := blob.Exists(q, uuid)
		if exists {
			// Remove from unclustered — this blob is now accounted for.
			if _, err := q.Exec("DELETE FROM unclustered WHERE rid=?", memberRID); err != nil {
				return fmt.Errorf("manifest.CrosslinkCluster unclustered delete rid=%d: %w", memberRID, err)
			}
		} else {
			// Create phantom for unknown UUID.
			if _, err := blob.StorePhantom(q, uuid); err != nil {
				return fmt.Errorf("manifest.CrosslinkCluster phantom %s: %w", uuid, err)
			}
		}
	}

	return nil
}
