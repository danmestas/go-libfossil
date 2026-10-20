package manifest

import (
	"container/heap"
	"context"
	"database/sql"
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
	"github.com/danmestas/go-libfossil/internal/leaf"
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
//
// The name carries a version because the table's verdicts are only as good as
// the parser that reached them. Revision 2 is the parser that reads S cards
// and sub-manifests (draft-fossil-submanifests-00): a repository whose
// earlier sweeps filed a tree manifest or sub-manifest here as unparseable
// must examine it again, so revision 1's table is dropped rather than read.
const nonArtifactTable = "crosslink_nonartifact_v2"

// supersededNonArtifactTables are the earlier revisions of nonArtifactTable.
var supersededNonArtifactTables = []string{"crosslink_nonartifact"}

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
	for _, old := range supersededNonArtifactTables {
		// Read before writing: this runs before every sweep, and the drop
		// is owed once per repository.
		var present int
		err := q.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?",
			old).Scan(&present)
		if err != nil {
			return fmt.Errorf("find %s table: %w", old, err)
		}
		if present == 0 {
			continue
		}
		if _, err := q.Exec("DROP TABLE " + old); err != nil {
			return fmt.Errorf("drop %s table: %w", old, err)
		}
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
// is a pure function of its own artifact; tag-propagation state, which
// depends on the plink graph rather than any one artifact, is deferred to
// the end of each batch's transaction, where repairTagPropagation starts from
// the parents the batch linked and the leaf rechecks its branch rows scheduled
// are applied (linkTx, batchSchedule, repairOnTx).
// Crosslink runs the sweep with no cancellation, supplying its own background
// context. It is the historical entry point; callers that can be interrupted
// (a clone bounded by a deadline) should use CrosslinkContext instead.
func Crosslink(r *repo.Repo) (int, error) {
	return CrosslinkContext(context.Background(), r)
}

// CrosslinkContext is a one-shot ReceiveLinker: callers that do not receive
// blobs incrementally get the same candidate ordering, cache, and per-batch
// repair gates as a clone, without retaining a linker beyond this call.
//
// Each sweep batch runs the repair gate before it commits (linkTx), as each
// phantom-fill cascade batch does (AfterDephantomize, AfterDephantomizeBatch),
// so every scheduled leaf recheck is applied before a later reader sees the
// leaf table (draft-fossil-repo-state-01 Section 7.3) and nothing is left
// for the end of the sweep, nor for a sync whose cascades linked everything
// (issue #414).
func CrosslinkContext(ctx context.Context, r *repo.Repo) (int, error) {
	if r == nil {
		panic("manifest.CrosslinkContext: r must not be nil")
	}
	linker, err := newReceiveLinker(r, crosslinkCacheBytes)
	if err != nil {
		return 0, err
	}
	return linker.Finalize(ctx)
}

// CrosslinkRebuild crosslinks every artifact for a terminal rebuild, entirely
// within the caller's transaction: candidate discovery, linking and the leaf and
// tag repair all run on tx, so a rebuild that fails partway rolls back to the
// derived state it started from, as fossil's rebuild_db does in its one
// transaction. The leaf table is the batch's own schedule applied once, which
// is Section 7.3's single-batch result. It links
// exactly what Crosslink would: a check-in whose file content or parent is
// absent is linked against phantoms, and a delta manifest whose baseline is
// absent is refused and filed under its baseline in orphan, as
// `fossil rebuild` does (src/manifest.c fetch_baseline; see missingBaseline).
func CrosslinkRebuild(tx *db.Tx) (int, error) {
	if tx == nil {
		panic("manifest.CrosslinkRebuild: tx must not be nil")
	}
	if err := prepareCrosslinkTables(tx); err != nil {
		return 0, err
	}
	candidates, err := collectCrosslinkCandidates(tx)
	if err != nil {
		return 0, err
	}
	st := newLinkState(crosslinkCacheBytes)
	if err := linkBatch(context.Background(), tx, candidates, st); err != nil {
		return 0, err
	}
	if st.linked == 0 {
		return 0, nil
	}
	// The batch's own schedule, not leaf.Rebuild: it is the single-batch
	// result Section 7.3 defines, and it reaches a check-in whose inherited
	// branch row a cancel deleted, which a scan of surviving rows cannot. The
	// batch linked every check-in, so its parents are every primary parent.
	if err := repairOnTx(tx, st.sched); err != nil {
		return st.linked, fmt.Errorf("manifest.CrosslinkRebuild: %w", err)
	}
	return st.linked, nil
}

// crosslinkSweepWithState performs only the candidate pass, sharing st's cache
// and schedule with the caller's receive-time links. Each batch runs its own
// repair gate (linkTx).
func crosslinkSweepWithState(ctx context.Context, r *repo.Repo, st *linkState) (int, error) {
	if r == nil {
		panic("manifest.crosslinkSweepWithState: r must not be nil")
	}
	if st == nil {
		panic("manifest.crosslinkSweepWithState: state must not be nil")
	}
	if err := prepareCrosslinkTables(r.DB()); err != nil {
		return 0, err
	}
	candidates, err := collectCrosslinkCandidates(r.DB())
	if err != nil {
		return 0, err
	}
	return linkCandidatesInOrder(ctx, r, candidates, st)
}

func prepareCrosslinkTables(q db.Querier) error {
	if q == nil {
		panic("manifest.prepareCrosslinkTables: q must not be nil")
	}
	if err := ensureForumPostTable(q); err != nil {
		return fmt.Errorf("manifest.Crosslink: %w", err)
	}
	if err := ensureNonArtifactTable(q); err != nil {
		return fmt.Errorf("manifest.Crosslink: %w", err)
	}
	return nil
}

// repairCrosslinkState runs the repair gate for links that committed without
// one, and returns cause joined with any repair failure. Every link this
// package makes runs its gate in its own transaction (linkTx), so sched is
// owed only when a caller ran ReceiveLinker.LinkStored in a transaction it
// opened itself rather than through ReceiveLinker.WithTx; otherwise this does
// nothing. sched is cleared once the gate commits, and returned to its extent
// before the gate when the gate rolls back, so a retry neither loses a
// recheck or a parent nor keeps one only the rolled-back gate scheduled.
func repairCrosslinkState(r *repo.Repo, sched *batchSchedule, cause error) error {
	if sched == nil {
		panic("manifest.repairCrosslinkState: sched must not be nil")
	}
	if !sched.owed {
		return cause
	}
	mark := sched.mark()
	err := r.WithTx(func(tx *db.Tx) error { return repairOnTx(tx, sched) })
	if err != nil {
		sched.rollback(mark)
		return errors.Join(cause, fmt.Errorf("manifest.Crosslink: %w", err))
	}
	sched.reset()
	return cause
}

// repairOnTx is the repair gate itself: the tag-propagation state that depends
// on the plink graph rather than on any one artifact, then the batch's
// scheduled leaf rechecks (draft-fossil-repo-state-00 Section 7.3, applied
// once per batch, before it commits). Only the check-ins a branch write
// scheduled are checked, so a
// check-in no branch write reached keeps the verdict an earlier batch gave it
// (issue #426). Propagation, likewise, starts only from the parents the batch
// gave a child (issue #432), so the work is bounded by the batch, not the
// repository.
func repairOnTx(tx *db.Tx, sched *batchSchedule) error {
	if sched == nil {
		panic("manifest.repairOnTx: sched must not be nil")
	}
	if err := repairPlinkAndTags(tx, sched); err != nil {
		return err
	}
	return sched.leaves.Apply(tx)
}

// repairPlinkAndTags reparents and propagates, scheduling on sched the
// rechecks the propagation's branch writes call for.
func repairPlinkAndTags(tx *db.Tx, sched *batchSchedule) error {
	// Reparent first, as fossil's manifest_crosslink_end does before it
	// propagates tags: the parent tag rewrites plink, and both the
	// propagation walk and the leaf checks after it must see the new edges.
	// Like fossil's manifest_reparent_checkin, rewriting plink schedules no
	// recheck of its own; the new parents join sched's, so the walk reaches
	// the check-ins moved under them.
	if err := ReparentCheckins(tx, &sched.parents); err != nil {
		return err
	}
	return repairTagPropagation(tx, sched)
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
	  AND (b.rid IN (SELECT o.rid FROM orphan o JOIN blob dep ON dep.rid=o.baseline WHERE dep.size>=0)
	    OR (b.rid NOT IN (SELECT objid FROM event WHERE objid IS NOT NULL)
	  AND b.rid NOT IN (SELECT srcid FROM tagxref WHERE srcid IS NOT NULL)
	  AND b.rid NOT IN (SELECT fpid FROM forumpost WHERE fpid IS NOT NULL)
	  AND b.rid NOT IN (SELECT attachid FROM attachment WHERE attachid IS NOT NULL)
	  AND b.rid NOT IN (SELECT rid FROM ` + nonArtifactTable + `)))
	ORDER BY b.rid
`

// collectCrosslinkCandidates returns every not-yet-crosslinked blob, ordered so
// that a delta's base is visited before the delta itself (see deltaChainOrder).
// It is read-only: the incremental sweep runs it on q outside any transaction,
// a rebuild inside its own.
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
// their baseline has not arrived (missingBaseline). A ReceiveLinker owns the
// same state from each accepted blob through its final candidate sweep.
type linkState struct {
	cache  *content.Cache
	guard  *checkinDeferralGuard
	linked int
	// sched is the current link transaction's leaf rechecks and the parents
	// its tags propagate from, both run by the repair gate before it commits
	// (linkTx).
	sched *batchSchedule
}

func newLinkState(cacheBytes int64) *linkState {
	if cacheBytes <= 0 {
		panic("manifest.newLinkState: cacheBytes must be > 0")
	}
	st := &linkState{
		cache: content.NewCache(cacheBytes),
		guard: newCheckinDeferralGuard(),
		sched: newBatchSchedule(),
	}
	st.guard.cache = st.cache
	return st
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
		if err := linkTx(r, st.sched, func(tx *db.Tx) error {
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
	// ReceiveLinker reuses its guard across receipt transactions and Finalize
	// calls. Raw aliases can change between them, so share proof only within
	// this transaction's unchanged batch, never across ingestion boundaries.
	st.guard.coneReferences = nil
	defer func() { st.guard.coneReferences = nil }()
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
		if err := noteTreeReferences(tx, c.rid, d); err != nil {
			return err
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
		if d.Type == deck.Checkin {
			deferred, err := deferCheckin(tx, c, d, st)
			if err != nil {
				return err
			}
			if deferred {
				linked, err := crosslinkPartialCheckin(ctx, tx, c.rid, d, st.guard, st.sched)
				if err != nil {
					return err
				}
				if linked {
					batchLinked++
				}
				continue
			}
		}
		handled, linkErr := linkArtifact(tx, c.rid, d, st.cache, st.sched)
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
		// Required: D, E, W, Z -- and every T-card a '+' (singleton) tag on the
		// technote itself. A technote whose T-card names another artifact, or
		// propagates or cancels, is refused outright; fossil rejects it at parse
		// time (src/manifest.c, "non-self-referential T-card in technote" and
		// "T-card with '*' or '-' in technote"). We gate it here so a technote the
		// parser accepts applies none of its tags (issue #287).
		return !d.D.IsZero() && d.E != nil && d.W != nil && technoteTagsSelf(d)
	case deck.ForumPost:
		return !d.D.IsZero() && d.U != nil && d.W != nil
	case deck.Submanifest:
		// A sub-manifest is an artifact with no crosslink effect of its own
		// (draft-fossil-submanifests-00 §7.3): it feeds repository state only
		// through the trees that contain it. Answering false files it with the
		// examined blobs, the explicit completion marker §9.2 requires, so a
		// sweep that finds work by the absence of derived rows never selects
		// it again. noteTreeReferences has already done what its arrival owes.
		return false
	default:
		return !d.D.IsZero()
	}
}

// noteTreeReferences does what storing an S-bearing artifact owes the
// repository, whatever path parsed it (sweep, receive linker or cascade): it
// sets the seen-submanifest indicator of draft-fossil-submanifests-00 §8.1,
// which arms the transfer gate of §7.2, and reserves a phantom for every S
// reference that has not arrived, with the referring artifact's privacy
// (§7.1), so the next sync round asks for it and a tree of depth d converges
// in d+1 rounds.
func noteTreeReferences(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("manifest.noteTreeReferences: tx must not be nil")
	}
	if len(d.S) == 0 && d.Type != deck.Submanifest {
		return nil
	}
	if err := MarkSeenSubmanifest(tx); err != nil {
		return err
	}
	private := content.IsPrivate(tx, int64(rid))
	for _, sc := range d.S {
		if _, ok := blob.Exists(tx, sc.UUID); ok {
			continue
		}
		phantom, err := blob.StorePhantom(tx, sc.UUID)
		if err != nil {
			return fmt.Errorf("phantom for S reference %s: %w", sc.UUID, err)
		}
		if !private {
			continue
		}
		if err := content.MakePrivate(tx, int64(phantom)); err != nil {
			return fmt.Errorf("private phantom for S reference %s: %w", sc.UUID, err)
		}
	}
	return nil
}

// NoteStoredArtifact is noteTreeReferences for a path that stores an
// artifact without crosslinking it at once -- a sync's receive, whose sweep
// runs only at the end of the session -- so the indicator, and with it the
// §7.2 transfer gate, is armed from the moment a tree is stored.
func NoteStoredArtifact(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) error {
	return noteTreeReferences(tx, rid, d)
}

// UnexaminedByThisParser reports whether an earlier version's sweep record
// survives, so the repository may hold trees that version filed as
// unparseable and nothing has examined since: until the next sweep drops
// that record, a caller must assume trees are present.
func UnexaminedByThisParser(q db.Querier) (bool, error) {
	for _, old := range supersededNonArtifactTables {
		var present int
		if err := q.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?",
			old).Scan(&present); err != nil {
			return false, fmt.Errorf("find %s table: %w", old, err)
		}
		if present > 0 {
			return true, nil
		}
	}
	return false, nil
}

// SeenSubmanifestSetting is the local indicator of
// draft-fossil-submanifests-00 §8.1: set once the repository has stored an
// S-bearing artifact, and never cleared, since such an artifact cannot be
// retired. It is not a /project setting and never syncs.
const SeenSubmanifestSetting = "seen-submanifest"

// MarkSeenSubmanifest sets the seen-submanifest indicator.
func MarkSeenSubmanifest(q db.Querier) error {
	if _, err := q.Exec(
		"INSERT OR IGNORE INTO config(name, value, mtime) VALUES(?, '1', strftime('%s','now'))",
		SeenSubmanifestSetting,
	); err != nil {
		return fmt.Errorf("seen-submanifest: %w", err)
	}
	return nil
}

// SeenSubmanifest reports whether the repository has stored an S-bearing
// artifact (MarkSeenSubmanifest).
func SeenSubmanifest(q db.Querier) (bool, error) {
	var one int
	err := q.QueryRow("SELECT 1 FROM config WHERE name=?", SeenSubmanifestSetting).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("seen-submanifest: %w", err)
	}
	return true, nil
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

// technoteTagsSelf reports whether every T-card on a technote is a '+'
// (singleton) tag applied to the technote itself ('*'). fossil requires both
// of a CFTYPE_EVENT artifact (src/manifest.c): a technote may only tag itself,
// and only with singleton tags.
func technoteTagsSelf(d *deck.Deck) bool {
	for _, tc := range d.T {
		if tc.Type != deck.TagSingleton || tc.UUID != "*" {
			return false
		}
	}
	return true
}

// linkArtifact writes the derived rows for one parsed artifact on tx. handled
// is false for artifact types the sweep does not link (so the caller does not
// count them), matching the old switch's `default: continue`. sched receives
// the leaf rechecks the artifact's branch tags schedule and the primary parents
// its plink rows name, and is marked owed for a handled artifact; the caller's
// repair gate runs all of it.
func linkArtifact(
	tx *db.Tx, rid libfossil.FslID, d *deck.Deck, cache *content.Cache, sched *batchSchedule,
) (handled bool, err error) {
	if sched == nil {
		panic("manifest.linkArtifact: sched must not be nil")
	}
	handled, err = linkArtifactRows(tx, rid, d, cache, sched)
	if handled && err == nil {
		sched.owed = true
	}
	return handled, err
}

// linkArtifactRows is linkArtifact's dispatch on the artifact's type.
func linkArtifactRows(
	tx *db.Tx, rid libfossil.FslID, d *deck.Deck, cache *content.Cache, sched *batchSchedule,
) (handled bool, err error) {
	switch d.Type {
	case deck.Checkin:
		return true, crosslinkCheckin(tx, rid, d, cache, sched)
	case deck.Wiki:
		return true, crosslinkWiki(tx, rid, d, &sched.parents)
	case deck.Ticket:
		return true, crosslinkTicket(tx, rid, d, cache)
	case deck.Event:
		return true, crosslinkEvent(tx, rid, d, sched)
	case deck.Attachment:
		return true, crosslinkAttachment(tx, rid, d)
	case deck.Cluster:
		return true, CrosslinkCluster(tx, rid, d)
	case deck.ForumPost:
		return true, crosslinkForum(tx, rid, d, &sched.parents)
	case deck.Control:
		return true, crosslinkControl(tx, rid, d, sched.leaves)
	}
	return false, nil
}

// checkinDeferralGuard is the accumulator state behind holding back a Checkin
// manifest whose B-card baseline (or, for the cascade, a file) is not yet
// available locally: one availability cache shared across every checkin
// it tests, exact deferred-reference counts for the end-of-run rollup, and
// bounded deterministic samples of deferred rids and missing blob UUIDs.
//
// Both the sweep and the phantom-fill cascade embed one. A deferred check-in
// remains discoverable through absent event rows, or through its orphan row
// when sparse spine metadata is published before complete file derivation.
type checkinDeferralGuard struct {
	avail *content.AvailabilityCache
	// cache expands the sub-manifests a tree's walk reads; it is the owning
	// sweep's or cascade's content cache, or nil, which expands directly.
	cache *content.Cache
	// Scoped to one linkBatch transaction; incoming raw aliases invalidate proof.
	coneReferences *content.ConeReferences
	// checkinBaselines memoizes baselines verified to be check-in manifests,
	// each mapped to whether it is a tree manifest, so a sweep over many
	// deltas parses each baseline once and walks only a tree's.
	checkinBaselines map[string]bool
	// awaitFiles makes the guard also wait on a check-in's files
	// (missingCheckinRefs), not only its baseline (missingBaseline). Only the
	// receive-time cascade sets it; a sweep never does.
	awaitFiles            bool
	deferredCount         int
	missingReferenceCount int
	deferredRids          []libfossil.FslID
	missingBlobs          map[string]struct{}
}

// newCheckinDeferralGuard returns a guard ready to test checkins for one
// sweep or one cascade run. Not safe for concurrent use; each caller owns one.
func newCheckinDeferralGuard() *checkinDeferralGuard {
	return &checkinDeferralGuard{
		avail:            content.NewAvailabilityCache(),
		checkinBaselines: make(map[string]bool),
		deferredRids:     make([]libfossil.FslID, 0, logDeferredSampleSize),
		missingBlobs:     make(map[string]struct{}, logDeferredSampleSize),
	}
}

// shouldDefer reports whether rid's checkin manifest d must be held back
// because its B-card baseline -- or, for a guard that awaits files, a file --
// has not arrived locally yet (see missingBaseline and missingCheckinRefs),
// recording the gap and the guard's diagnostics when so. source labels the
// caller in the debug log ("Crosslink" or "AfterDephantomize").
//
// The manifest blob remains durable in 'blob'. The orphan row records incomplete
// file derivation and keeps the check-in eligible for retry after its dependency
// arrives, even if metadata exists. Callers must not derive mlinks on deferral;
// only crosslinkPartialCheckin may publish independently validated spine metadata.
func (g *checkinDeferralGuard) shouldDefer(
	tx *db.Tx, source string, rid libfossil.FslID, d *deck.Deck,
) (bool, error) {
	var missing []string
	if g.awaitFiles {
		missing = missingCheckinRefs(tx, d, g)
	} else {
		missing = missingFileSet(tx, d, g)
	}
	if len(missing) == 0 {
		return false, nil
	}
	return true, g.recordDeferred(tx, source, rid, d, missing)
}

// recordDeferred records the baseline gap, if any, of checkin rid (deck d)
// whose missing references have already been computed, and retains exact
// diagnostic counts and bounded samples for it. An absent baseline is
// missing[0] (see missingCheckinRefs).
func (g *checkinDeferralGuard) recordDeferred(
	tx *db.Tx, source string, rid libfossil.FslID, d *deck.Deck, missing []string,
) error {
	if len(missing) == 0 {
		panic("manifest.recordDeferred: missing must not be empty")
	}
	if gap := g.gapOf(tx, d, missing); gap != "" {
		if err := recordBaselineGap(tx, rid, gap); err != nil {
			return fmt.Errorf("manifest.%s defer rid=%d: %w", source, rid, err)
		}
	}
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
	return nil
}

// gapOf returns the reference among missing that gets a gap record: the
// baseline, or the sub-manifest that blocks the check-in's tree. Either one,
// when absent, is missing[0] (missingFileSet runs first). A waited-on file
// gets none: it is not a gap in the artifact, only content not yet arrived.
func (g *checkinDeferralGuard) gapOf(tx *db.Tx, d *deck.Deck, missing []string) string {
	if d.B != "" && missing[0] == d.B {
		return d.B
	}
	if gap := g.treeGap(tx, d); gap != "" && gap == missing[0] {
		return gap
	}
	if gap := g.parentTreeGap(tx, d); gap != "" && gap == missing[0] {
		return gap
	}
	return ""
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
// its baseline has not arrived locally yet (see missingBaseline). Thin wrapper
// over checkinDeferralGuard.shouldDefer for linkBatch's call site.
func deferCheckin(tx *db.Tx, c candidate, d *deck.Deck, st *linkState) (bool, error) {
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

// repairTagPropagation re-runs tag propagation from every primary parent the
// batch wrote a plink row for (sched.parents), once those plink edges are in
// place, at the end of the transaction that wrote them (linkTx).
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
//
// Re-propagating from a parent whose children already hold its rows writes
// nothing (the strict mtime test above), so only a real change to a branch
// row schedules a leaf recheck on sched.leaves.
//
// The batch's parents are the only ones that can owe a write (issue #432). A
// parent's tags reach its children when the gate after their plink row
// propagates from it, or when a tag lands on it (tag_insert propagates from
// its target, tag.ApplyTagWithTx), and the walk carries both on down. A parent
// no plink row of this batch names gained no child since a gate that already
// propagated from it, so it is skipped. A rebuild links every check-in, so its
// batch holds every primary parent, as the whole-repository pass did.
func repairTagPropagation(q db.Querier, sched *batchSchedule) error {
	if q == nil {
		panic("manifest.repairTagPropagation: q must not be nil")
	}
	if sched == nil {
		panic("manifest.repairTagPropagation: sched must not be nil")
	}
	for _, pid := range sched.parents.sorted() {
		if err := propagateAllLeaves(q, pid, sched.leaves); err != nil {
			return fmt.Errorf("repairTagPropagation propagate rid=%d: %w", pid, err)
		}
	}
	return nil
}

// propagateAllLeaves is the repair gate's propagation step. It is a variable
// so a test can count the parents a gate visits (issue #432); a test that
// replaces it must not run in parallel with any other manifest test.
var propagateAllLeaves = tag.PropagateAllLeaves

// missingBaseline returns the reference whose absence holds a check-in
// manifest back from a sweep: the B-card baseline of a delta manifest, when
// that baseline's content is not available. The result is empty when the
// check-in may be linked now.
//
// The baseline is the only such reference, and it must be a check-in manifest,
// not merely available content. Without it the delta manifest has
// no file set, so [FossilArtifact] Section 8.3 makes its crosslink a soft
// failure retried once the baseline arrives, and fossil's manifest_crosslink
// refuses it outright (fetch_baseline fails; src/manifest.c ~1363, ~2361).
// That holds for a rebuild too: fossil rebuild refuses it the same way.
//
// Absent file content and an absent parent never hold a check-in back from a
// sweep. Each is an unresolved reference, which yields a phantom rather than
// an error (draft-fossil-repo-state-00 Section 4.2 step 1): fossil's
// add_one_mlink resolves files with uuid_to_rid(..., 1), so the event, plink
// and mlink rows are all written, against phantoms that the content fills in
// place when it arrives. A sweep is the last step of every sync, clone and
// rebuild, so holding a check-in there for a file that never arrives withheld
// its rows forever (#406). Only the receive-time paths, which run while more
// blobs are still arriving, wait on files too; see missingCheckinRefs.
//
// A deferred check-in gets no event row, so the candidate query re-selects it
// on the next sweep; recordBaselineGap also files it under its baseline in
// orphan, where the phantom-fill cascade finds it when the baseline arrives.
func missingBaseline(tx *db.Tx, d *deck.Deck, g *checkinDeferralGuard) []string {
	if tx == nil {
		panic("manifest.missingBaseline: tx must not be nil")
	}
	if d == nil {
		panic("manifest.missingBaseline: d must not be nil")
	}
	if d.B == "" {
		return nil
	}
	if _, ok := g.avail.ByUUID(tx, d.B); !ok {
		return []string{d.B}
	}
	if !g.baselineIsCheckin(tx, d.B) {
		return []string{d.B}
	}
	return nil
}

// missingFileSet is missingBaseline widened to the third deferred-edge kind of
// draft-fossil-submanifests-00 §9.4: a check-in whose tree -- its own, or its
// baseline's for a delta over a tree -- cannot be enumerated waits on the
// sub-manifest that blocks the walk, exactly as a delta waits on its baseline.
// §5.2 forbids presenting a partial file set, so a sweep defers on it too.
func missingFileSet(tx *db.Tx, d *deck.Deck, g *checkinDeferralGuard) []string {
	if missing := missingBaseline(tx, d, g); len(missing) > 0 {
		return missing
	}
	if gap := g.treeGap(tx, d); gap != "" {
		return []string{gap}
	}
	if gap := g.parentTreeGap(tx, d); gap != "" {
		return []string{gap}
	}
	return nil
}

// treeGap returns the sub-manifest reference that stops check-in d's tree
// from enumerating, or "" when it enumerates or d has no tree. A missing
// sub-manifest is the usual answer; a reference that breaks a rule of §5.1 is
// returned too, as a baseline that is no check-in is by missingBaseline: the
// check-in waits on bytes that will never make it valid, and is never linked
// with a partial file set. Its baseline, when it has one, must be available.
func (g *checkinDeferralGuard) treeGap(tx *db.Tx, d *deck.Deck) string {
	tree := d
	if d.B != "" {
		// missingBaseline, which runs first, recorded whether the baseline
		// is a tree; a flat one leaves nothing to walk.
		if baseIsTree, known := g.checkinBaselines[d.B]; known && !baseIsTree {
			return ""
		}
		baseDeck, err := loadBaseline(tx, g.cache, d)
		if err != nil {
			return d.B
		}
		tree = baseDeck
	}
	if !isTree(tree) {
		return ""
	}
	_, err := walkTree(tx, g.cache, tree)
	if err == nil {
		return ""
	}
	var treeErr *SubmanifestError
	if errors.As(err, &treeErr) {
		return treeErr.UUID
	}
	return tree.S[0].UUID
}

// baselineIsCheckin reports whether the available artifact uuid parses as a
// check-in manifest. fossil's fetch_baseline asks manifest_get(rid,
// CFTYPE_MANIFEST), which fails for a stored artifact of another type just as
// it does for an absent one, and [FossilArtifact] Section 8.3 treats "not a
// manifest" like "cannot be located". Only a positive answer is memoized:
// the verdict for a given blob never changes, but a sweep reaches many deltas
// over few baselines, so one expansion per baseline is enough.
func (g *checkinDeferralGuard) baselineIsCheckin(tx *db.Tx, uuid string) bool {
	if _, ok := g.checkinBaselines[uuid]; ok {
		return true
	}
	rid, ok := g.avail.ByUUID(tx, uuid)
	if !ok {
		return false
	}
	data, err := expandManifestBytes(tx, nil, rid)
	if err != nil {
		return false
	}
	bd, err := deck.Parse(data)
	if err != nil || bd.Type != deck.Checkin {
		return false
	}
	g.checkinBaselines[uuid] = isTree(bd)
	return true
}

// missingCheckinRefs is missingBaseline for the receive-time paths -- the
// phantom-fill cascade and the clone's ReceiveLinker -- which run while a
// session is still delivering blobs. Beyond the baseline it also waits on the
// check-in's (non-deleted) F-card files, the content Checkout.Update will
// read, so a check-in whose files are still in flight is not exposed half
// stored (agent-infra trial #10). That wait is bounded: every such session
// ends in a sweep, which links what is still waiting against phantoms. The
// baseline, when absent, is always first in the result.
//
// Files are not awaited when the primary parent's content is itself absent:
// with no parent manifest there is no file transition to derive -- fossil's
// add_mlink returns before reading a file -- and the parent's own crosslink
// derives the rows once it arrives (issue #398).
func missingCheckinRefs(tx *db.Tx, d *deck.Deck, g *checkinDeferralGuard) []string {
	missing := missingFileSet(tx, d, g)
	if len(d.P) > 0 {
		if _, ok := g.avail.ByUUID(tx, d.P[0]); !ok {
			return missing
		}
	}
	files := d.F
	if isTree(d) {
		// A tree's files are known only once it enumerates; until then the
		// sub-manifest it waits on is all there is to wait for.
		if len(missing) > 0 {
			return missing
		}
		walked, err := walkTree(tx, g.cache, d)
		if err != nil {
			return missing
		}
		files = walked
	}
	seen := make(map[string]struct{}, len(files))
	for _, f := range files {
		if f.UUID == "" { // a deleted file in a delta manifest
			continue
		}
		if _, dup := seen[f.UUID]; dup {
			continue
		}
		seen[f.UUID] = struct{}{}
		if _, ok := g.avail.ByUUID(tx, f.UUID); !ok {
			missing = append(missing, f.UUID)
		}
	}
	return missing
}

// recordBaselineGap records that check-in rid waits on the baseline named
// baseline, as fossil's fetch_baseline does when it refuses a delta manifest:
// the baseline resolves to its rid, reserving a phantom when it has never been
// seen, so a sync requests it, and an orphan row keys rid by that baseline, so
// the phantom-fill cascade (afterDephantomize) links rid as soon as it lands.
// This is the gap record of [FossilArtifact] Section 8.3's soft failure.
func recordBaselineGap(tx *db.Tx, rid libfossil.FslID, baseline string) error {
	if tx == nil {
		panic("manifest.recordBaselineGap: tx must not be nil")
	}
	if rid <= 0 {
		panic("manifest.recordBaselineGap: rid must be positive")
	}
	if baseline == "" {
		panic("manifest.recordBaselineGap: baseline must not be empty")
	}
	baseRid, err := ridOrPhantom(tx, baseline)
	if err != nil {
		return fmt.Errorf("baseline %s: %w", baseline, err)
	}
	if _, err := tx.Exec(
		"INSERT INTO orphan(rid, baseline) VALUES(?, ?) ON CONFLICT(rid) DO UPDATE SET baseline=excluded.baseline", rid, baseRid,
	); err != nil {
		return fmt.Errorf("orphan rid=%d: %w", rid, err)
	}
	return nil
}

// crosslinkCheckin links one check-in manifest. cache is the caller's shared
// content cache -- the whole-sweep one, or the one cascadeLinker runs for a
// phantom-fill cascade; insertCheckinMlinks uses it to expand the parent
// manifest at most once per parent across the run. A nil cache is legal and
// expands every parent from scratch, but no caller passes one.
func crosslinkCheckin(
	tx *db.Tx, rid libfossil.FslID, d *deck.Deck, cache *content.Cache, sched *batchSchedule,
) error {
	if tx == nil {
		panic("crosslinkCheckin: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkCheckin: rid must be positive")
	}

	if err := crosslinkCheckinTables(tx, rid, d, cache, &sched.parents); err != nil {
		return err
	}
	// A linked check-in is no longer waiting on a baseline. Whichever path
	// links it -- sweep, receive linker or cascade -- drops the gap row, as
	// fossil's after_dephantomize does once the baseline lands.
	if _, err := tx.Exec("DELETE FROM orphan WHERE rid=?", rid); err != nil {
		return fmt.Errorf("orphan: %w", err)
	}
	if err := recheckTaggedBeforeLinking(tx, rid, sched.leaves); err != nil {
		return err
	}
	return applyInlineTags(tx, rid, d, sched.leaves)
}

// recheckTaggedBeforeLinking schedules rid, and with it the parents its plink
// rows now name, when rid already carries a branch row: one a control
// artifact or a propagation wrote before rid was linked. That write scheduled
// rid while it had no parents, so a parent on the same branch kept a leaf row
// it no longer earns. Fossil schedules nothing here (adding a plink row is
// not a branch write) and shares the gap across rounds, but applies its
// rechecks once per round, where an out-of-order clone rarely hits it; each
// link transaction here applies its own (linkTx), and a receive links one
// artifact per transaction, so the gap would be common. A check-in with no
// branch row is left alone, as fossil leaves it (issue #426).
func recheckTaggedBeforeLinking(tx *db.Tx, rid libfossil.FslID, leaves *leaf.Pending) error {
	var tagged int
	err := tx.QueryRow("SELECT count(*) FROM tagxref WHERE rid=? AND tagid=?",
		rid, leaf.BranchTagID).Scan(&tagged)
	if err != nil {
		return fmt.Errorf("branch row rid=%d: %w", rid, err)
	}
	if tagged == 0 {
		return nil
	}
	return leaves.Schedule(tx, rid)
}

// crosslinkCheckinTables populates event/plink/mlink/cherrypick on tx,
// recording the primary parent on parents for the repair gate to propagate
// from. leaf is
// deliberately not touched here: adding a plink row schedules no recheck
// (draft-fossil-repo-state-00 Section 7.3); only a branch row does, through
// applyInlineTags and the repair gate's propagation. tx is the whole
// sweep's single transaction (see CrosslinkContext), so these writes commit
// atomically with every other candidate's, not one transaction per checkin.
func crosslinkCheckinTables(
	tx *db.Tx, rid libfossil.FslID, d *deck.Deck, cache *content.Cache, parents *parentSet,
) error {
	if err := crosslinkCheckinMetadata(tx, rid, d, parents); err != nil {
		return err
	}
	return insertCheckinMlinks(tx, cache, rid, d)
}

func crosslinkCheckinMetadata(
	tx *db.Tx, rid libfossil.FslID, d *deck.Deck, parents *parentSet,
) error {
	// event, at the D-card time; omtime always keeps it. A date tag already
	// recorded for the check-in (one that reached it while it was a phantom,
	// or a rebuild that linked the tag first) then moves mtime through
	// tag.RefreshEventTime, the same rule a tag arriving later applies, so
	// the time does not depend on arrival order. Unlike manifest_crosslink,
	// that rule skips an inherited date row (#372) and a cancelled, unparsable
	// or non-positive value (draft-fossil-repo-state-00 Section 4.2, step 6).
	mtime := libfossil.TimeToTextJulian(d.D)
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO event(type, mtime, objid, user, comment, omtime,
		   bgcolor, euser, ecomment)
		 VALUES('ci', ?1, ?2, ?3, ?4, ?1,
		   (SELECT value FROM tagxref WHERE rid=?2 AND tagtype>0
		     AND tagid=(SELECT tagid FROM tag WHERE tagname='bgcolor')),
		   (SELECT value FROM tagxref WHERE rid=?2
		     AND tagid=(SELECT tagid FROM tag WHERE tagname='user')),
		   (SELECT value FROM tagxref WHERE rid=?2
		     AND tagid=(SELECT tagid FROM tag WHERE tagname='comment')))`,
		mtime, rid, d.U, sql.NullString{String: d.C, Valid: d.HasC()},
	); err != nil {
		return fmt.Errorf("event: %w", err)
	}
	if err := tag.RefreshEventTime(tx, rid); err != nil {
		return fmt.Errorf("event time: %w", err)
	}

	if err := insertCheckinPlinks(tx, rid, d, checkinBaseid(tx, d), parents); err != nil {
		return err
	}
	return insertCherrypicks(tx, rid, d)
}

// checkinBaseid resolves a delta check-in's baseline (B-card) to the blob rid
// the plink baseid column carries, or nil for a baseline manifest or a
// baseline blob that has not arrived. It is the one place both the normal
// linkage path and the reparent pass read the baseline from.
func checkinBaseid(q db.Querier, d *deck.Deck) any {
	if d.B == "" {
		return nil
	}
	var baseRid int64
	if err := q.QueryRow("SELECT rid FROM blob WHERE uuid=?", d.B).Scan(&baseRid); err != nil {
		return nil
	}
	return baseRid
}

// insertCheckinPlinks inserts plink rows for each parent (P-card) and records
// the primary parent on parents, as manifest_crosslink seeds tag_propagate_all
// with it (src/manifest.c).
func insertCheckinPlinks(
	tx *db.Tx, rid libfossil.FslID, d *deck.Deck, baseid any, parents *parentSet,
) error {
	if parents == nil {
		panic("manifest.insertCheckinPlinks: parents must not be nil")
	}
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
			parentRid, rid, isPrim, libfossil.TimeToTextJulian(d.D), baseid,
		); err != nil {
			return fmt.Errorf("plink: %w", err)
		}
		if isPrim == 1 {
			parents.add(libfossil.FslID(parentRid))
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
func applyInlineTags(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, leaves *leaf.Pending) error {
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
			Leaves:    leaves,
		}); err != nil {
			return fmt.Errorf("inline tag %q: %w", tc.Name, err)
		}
	}

	return nil
}

func crosslinkControl(tx *db.Tx, srcRID libfossil.FslID, d *deck.Deck, leaves *leaf.Pending) error {
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
			Leaves:    leaves,
		}); err != nil {
			return fmt.Errorf("apply tag %q to rid=%d: %w", tc.Name, targetRID, err)
		}
	}

	if err := tag.InsertControlEvent(tx, srcRID, d); err != nil {
		return fmt.Errorf("control event: %w", err)
	}

	return nil
}

// addFWTPlink inserts plink rows for wiki/forum/technote/ticket P-cards.
// Shared helper for artifact types that use P-cards (parents) but not the
// full checkin flow.
//
// It used to also call tag.PropagateAll from the primary parent here, for
// the same reason applyInlineTags did (see its comment): it records the parent
// on parents instead, and repairTagPropagation propagates from it once, after
// the sweep, as fossil's manifest_add_fwt_plink does at once.
func addFWTPlink(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, parents *parentSet) error {
	if tx == nil {
		panic("manifest.addFWTPlink: tx must not be nil")
	}
	if rid <= 0 {
		panic("manifest.addFWTPlink: rid must be positive")
	}
	if parents == nil {
		panic("manifest.addFWTPlink: parents must not be nil")
	}

	mtime := libfossil.TimeToTextJulian(d.D)

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
		if isPrim == 1 {
			parents.add(libfossil.FslID(parentRid))
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

func crosslinkWiki(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, parents *parentSet) error {
	if tx == nil {
		panic("crosslinkWiki: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkWiki: rid must be positive")
	}

	if err := addFWTPlink(tx, rid, d, parents); err != nil {
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
		libfossil.TimeToTextJulian(d.D), rid, d.U, comment,
	); err != nil {
		return fmt.Errorf("wiki event: %w", err)
	}

	return nil
}

// writeTechNoteEvent writes the event row of tech note version rid, the
// latest crosslinked so far, replacing the row of any earlier version.
func writeTechNoteEvent(tx *db.Tx, rid libfossil.FslID, tagid int64, d *deck.Deck) error {
	if d.E == nil {
		panic("manifest.writeTechNoteEvent: no E card")
	}
	if len(d.P) > 0 {
		if _, err := tx.Exec(`DELETE FROM event WHERE type='e' AND tagid=?
			AND objid IN (SELECT rid FROM tagxref WHERE tagid=?)`, tagid, tagid); err != nil {
			return fmt.Errorf("event delete: %w", err)
		}
	}
	var bgcolor any
	var bgStr string
	err := tx.QueryRow(`SELECT value FROM tagxref JOIN tag USING(tagid)
		WHERE tagname='bgcolor' AND rid=?`, rid).Scan(&bgStr)
	if err == nil {
		bgcolor = bgStr
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("event bgcolor: %w", err)
	}
	if _, err := tx.Exec(`REPLACE INTO event(type, mtime, objid, tagid, user, comment, bgcolor)
		VALUES('e', ?, ?, ?, ?, ?, ?)`,
		libfossil.TimeToTextJulian(d.E.Date), rid, tagid, d.U, d.C, bgcolor,
	); err != nil {
		return fmt.Errorf("event insert: %w", err)
	}
	return nil
}

// subsequentEventVersion returns a later version of tech note tagid than
// rid, or 0. Fossil compares against the time as it writes it in SQL text.
func subsequentEventVersion(
	tx *db.Tx, tagid int64, rid libfossil.FslID, d *deck.Deck,
) (int64, error) {
	if d == nil {
		panic("manifest.subsequentEventVersion: nil deck")
	}
	var subsequent int64
	err := tx.QueryRow(
		"SELECT rid FROM tagxref WHERE tagid=? AND mtime>=? AND rid!=? ORDER BY mtime LIMIT 1",
		tagid, libfossil.TimeToTextJulian(d.D), rid).Scan(&subsequent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("subsequent event: %w", err)
	}
	return subsequent, nil
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

func crosslinkEvent(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, sched *batchSchedule) error {
	if tx == nil {
		panic("crosslinkEvent: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkEvent: rid must be positive")
	}

	if d.E == nil {
		return fmt.Errorf("event manifest missing E-card")
	}
	if err := addFWTPlink(tx, rid, d, &sched.parents); err != nil {
		return fmt.Errorf("event plink: %w", err)
	}
	// A technote's T-cards are applied exactly as a check-in's or a control
	// artifact's are -- canonical Fossil runs all three through one block
	// (src/manifest.c, CFTYPE_CONTROL || CFTYPE_MANIFEST || CFTYPE_EVENT). This
	// has to happen before the event row is written below, because that row
	// reads the technote's own bgcolor tag back out of tagxref.
	if err := applyInlineTags(tx, rid, d, sched.leaves); err != nil {
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

	subsequent, err := subsequentEventVersion(tx, tagid, rid, d)
	if err != nil {
		return err
	}

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
	if subsequent == 0 {
		if err := writeTechNoteEvent(tx, rid, tagid, d); err != nil {
			return err
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
	mtime := libfossil.TimeToTextJulian(d.D)
	src, target, filename := d.A.Source, d.A.Target, d.A.Filename

	if _, err := tx.Exec(
		"INSERT INTO attachment(attachid, mtime, src, target, filename, comment, user) VALUES(?, ?, ?, ?, ?, ?, ?)",
		rid, mtime, sql.NullString{String: src, Valid: src != ""}, target, filename, d.C, d.U,
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

	human, url := libfossil.HashDigits(tx)
	evComment := attachmentComment(attachToType, src, filename, target, false, human, url)
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
		attachid         int64
		src              sql.NullString
		target, filename string
	}
	rows, err := tx.Query("SELECT attachid, src, target, filename FROM attachment WHERE target=?", targetID)
	if err != nil {
		return fmt.Errorf("updateAttachmentComments query: %w", err)
	}
	var attachments []attachRow
	for rows.Next() {
		var a attachRow
		if err := rows.Scan(&a.attachid, &a.src, &a.target, &a.filename); err != nil {
			rows.Close()
			return fmt.Errorf("updateAttachmentComments scan: %w", err)
		}
		attachments = append(attachments, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("updateAttachmentComments rows: %w", err)
	}
	rows.Close()

	human, url := libfossil.HashDigits(tx)
	for _, a := range attachments {
		comment := attachmentComment(targetType, a.src.String, a.filename, a.target, true, human, url)
		if _, err := tx.Exec("UPDATE event SET comment=?, type=? WHERE objid=?", comment, string(targetType), a.attachid); err != nil {
			return fmt.Errorf("updateAttachmentComments event update: %w", err)
		}
	}
	return nil
}

// attachmentComment builds the comment of an attachment's event row as
// fossil 2.28 does, for an attachment to a wiki page ('w'), a ticket ('t')
// or a tech note ('e'); an empty src deletes. refresh selects the text
// written when the ticket or tech note is crosslinked after the attachment,
// which differs for a tech note deletion: crosslinking the attachment links
// the file name as though it were an artifact hash, refreshing does not.
func attachmentComment(kind byte, src, file, target string, refresh bool, human, url int) string {
	if kind != 'w' && kind != 't' && kind != 'e' {
		panic(fmt.Sprintf("manifest.attachmentComment: unknown target kind %q", kind))
	}
	name := libfossil.HTMLize(file)
	switch {
	case kind == 'w' && src != "":
		return fmt.Sprintf("Add attachment [/artifact/%s|%s] to wiki page [%s]",
			libfossil.HashPrefix(src, url), name, libfossil.HTMLize(target))
	case kind == 'w':
		return fmt.Sprintf("Delete attachment \"%s\" from wiki page [%s]", name, libfossil.HTMLize(target))
	}
	noun, link := "ticket", fmt.Sprintf("[%s|%s]", libfossil.HashPrefix(target, url), libfossil.HashPrefix(target, human))
	if kind == 'e' {
		noun, link = "tech note", "[/technote/"+link[1:]
	}
	switch {
	case src != "":
		return fmt.Sprintf("Add attachment [/artifact/%s|%s] to %s %s", libfossil.HashPrefix(src, url), name, noun, link)
	case kind == 'e' && !refresh:
		return fmt.Sprintf("Delete attachment \"/artifact/%s|%s\" from %s %s",
			libfossil.HashPrefix(file, url), name, noun, link)
	default:
		return fmt.Sprintf("Delete attachment \"%s\" from %s %s", name, noun, link)
	}
}

func crosslinkForum(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, parents *parentSet) error {
	if tx == nil {
		panic("crosslinkForum: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkForum: rid must be positive")
	}

	if err := addFWTPlink(tx, rid, d, parents); err != nil {
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
		rid, froot, fprev, firt, libfossil.TimeToTextJulian(d.D),
	); err != nil {
		return fmt.Errorf("forumpost insert: %w", err)
	}

	mtime := libfossil.TimeToTextJulian(d.D)

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
