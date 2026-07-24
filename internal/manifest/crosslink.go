package manifest

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

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

// crosslinkCacheBytes bounds the expanded content one Crosslink sweep keeps
// live. A miss costs throughput, not correctness: the walk simply continues
// further back toward the chain root.
//
// Candidates are visited in delta-chain order (see deltaChainOrder), not
// ascending rid: every base a candidate needs is expanded at most one
// candidate-visit earlier, never arbitrarily far in the future. That bounds
// the working set by how many chains are in flight at once -- how many
// distinct files or manifests are being interleaved -- rather than by
// repository size, so this budget only has to cover that concurrency, not
// the whole repository expanded (8 GiB for the Fossil SCM repository under
// the old ascending-rid order this replaced).
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

type pendingItem struct {
	Type byte // 'w' = wiki backlink, 't' = ticket rebuild
	ID   string
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

// CrosslinkContext is Crosslink that observes ctx: a cancelled context aborts
// the candidate sweep within crosslinkCancelCheckStride candidates, returning
// the count linked so far and ctx.Err(). This is what lets a clone deadline
// interrupt the whole-repository crosslink pass, which has no round boundary of
// its own to fall back on.
func CrosslinkContext(ctx context.Context, r *repo.Repo) (int, error) {
	if r == nil {
		panic("manifest.CrosslinkContext: r must not be nil")
	}

	// The whole sweep runs inside one transaction. Per-candidate transactions
	// made every SQL statement open its own WAL read-transaction, whose setup
	// cost climbs as the uncheckpointed WAL grows -- the #89 collapse from
	// hundreds of artifacts per second to a handful within a few seconds of a
	// large sweep. One transaction sets the read point once and commits once,
	// matching Fossil's own rebuild_db and the single-tx shape of
	// internal/verify.Rebuild. Crosslink output is derived data, so rolling
	// the whole sweep back on interruption is correct: the next sync re-runs
	// it from the same durable blobs.
	return crosslinkSweep(ctx, r)
}

// crosslinkSweep discovers every uncrosslinked artifact, links them in
// delta-chain order, and repairs the order-sensitive derived state.
//
// The link pass commits in batches of crosslinkBatchSize candidates rather
// than in one repository-wide transaction. In WAL mode every page read first
// probes the wal-index for a newer frame, so a single transaction that writes
// every artifact's rows lets the WAL grow to hundreds of thousands of frames
// and every read -- even of unmodified blob and delta pages -- slows as that
// index grows, which collapsed throughput partway through a large sweep.
// Committing each batch lets SQLite checkpoint and truncate the WAL, holding
// wal-index probes flat. Per-candidate transactions (the state this replaces)
// paid a fresh WAL read-transaction setup on every statement instead; a batch
// amortizes that setup across thousands of candidates while still bounding the
// WAL. Crosslink output is derived data, so batch-granular durability is
// correct: an interrupted sweep leaves committed batches in place and the next
// candidate query re-selects only what is still unlinked.
func crosslinkSweep(ctx context.Context, r *repo.Repo) (int, error) {
	if r == nil {
		panic("manifest.crosslinkSweep: r must not be nil")
	}

	// Canonical fossil creates forumpost on demand -- only when a forum
	// artifact first requires it -- which is why `fossil rebuild` can drop
	// it for a repository that never had one. The candidate query below
	// names the table unconditionally, so a repository straight out of a
	// canonical rebuild needs it recreated before the sweep can run.
	if err := ensureForumPostTable(r.DB()); err != nil {
		return 0, fmt.Errorf("manifest.Crosslink: %w", err)
	}

	candidates, err := collectCrosslinkCandidates(r.DB())
	if err != nil {
		return 0, err
	}

	linked, sweepErr := linkCandidatesInOrder(ctx, r, candidates)

	// Repair pass: recompute the state that depends on every plink edge and
	// tagxref origin this sweep wrote being present, none of which can be
	// guaranteed mid-sweep once candidates are visited in delta-chain order
	// instead of ancestors-before-descendants. Mirrors Fossil's own
	// manifest_crosslink_end + leaf_rebuild() shape: defer the order-sensitive
	// work and repair it once, rather than trying to preserve order through the
	// sweep itself.
	//
	// Gated on linked > 0, not on sweepErr == nil: an interrupted sweep -- a
	// cancelled clone deadline, or an artifact-level error -- still committed
	// every batch before the one it stopped in (batches commit as they go),
	// and those batches wrote plink edges and tagxref origins whose leaf/tag
	// consequences are now stale. Repairing here on the way out closes that
	// window; skipping it would leave leaf/tag stale until a later sweep links
	// something, and a later sweep whose remaining candidates all link zero
	// artifacts would skip this same gate and leave the window open longer than
	// one sweep in an unlucky ordering (issue #143). A truly no-op sweep
	// (linked == 0) cannot have changed either repair's inputs, so it still
	// skips. The repair runs in its own context-free transaction, so a
	// cancelled ctx interrupts the candidate sweep but not this recovery.
	if linked > 0 {
		if err := r.WithTx(func(tx *db.Tx) error {
			if err := repairLeafTable(tx); err != nil {
				return err
			}
			return repairTagPropagation(tx)
		}); err != nil {
			// The interruption cause is the primary error to surface; a repair
			// failure layered on top of it is secondary. When the sweep itself
			// succeeded (sweepErr == nil), the repair failure is the error. When
			// the sweep was interrupted, join both so a real repair failure
			// (disk full, corruption, constraint violation) during a cancelled
			// sweep is still surfaced rather than silently dropped, while
			// errors.Is(err, context.Canceled) keeps holding for callers.
			if sweepErr != nil {
				return linked, errors.Join(sweepErr, fmt.Errorf("manifest.Crosslink: %w", err))
			}
			return linked, fmt.Errorf("manifest.Crosslink: %w", err)
		}
	}

	return linked, sweepErr
}

// collectCrosslinkCandidates returns every not-yet-crosslinked blob, ordered so
// that a delta's base is visited before the delta itself (see deltaChainOrder).
// It is read-only, so it runs on q outside any transaction.
func collectCrosslinkCandidates(q db.Querier) ([]candidate, error) {
	// Pass 1: Discover and crosslink all uncrosslinked artifacts.
	// ORDER BY b.rid here only seeds deltaChainOrder's tie-break, not the
	// final visiting order -- but it must still be deterministic input:
	// deferred manifests re-discovered across sweeps need a stable order
	// downstream of it. Without it, two syncs delivering the same blobs in
	// different arrival orders could produce divergent per-defer slog
	// streams and pending-item processing orders, masking determinism bugs
	// in downstream code.
	rows, err := q.Query(`
		SELECT b.rid, b.uuid FROM blob b
		WHERE b.size >= 0
		  AND NOT EXISTS (SELECT 1 FROM event e WHERE e.objid = b.rid)
		  AND NOT EXISTS (SELECT 1 FROM tagxref tx WHERE tx.srcid = b.rid)
		  AND NOT EXISTS (SELECT 1 FROM forumpost fp WHERE fp.fpid = b.rid)
		  AND NOT EXISTS (SELECT 1 FROM attachment a WHERE a.attachid = b.rid)
		ORDER BY b.rid
	`)
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
// every chain, and the set of checkins deferred because a referenced blob has
// not arrived. It is threaded through the per-batch transactions so a chain
// expanded in one batch stays cached for its dependents in later batches.
type linkState struct {
	cache        *content.Cache
	avail        *content.AvailabilityCache
	linked       int
	deferredRids []libfossil.FslID
	missingBlobs map[string]struct{}
	pending      []pendingItem
}

// linkCandidatesInOrder expands and crosslinks every candidate in delta-chain
// order, committing once per crosslinkBatchSize candidates, and returns the
// number of artifacts linked.
func linkCandidatesInOrder(ctx context.Context, r *repo.Repo, candidates []candidate) (int, error) {
	// One cache for the whole sweep, outliving the per-batch transactions.
	// Candidate rids were snapshotted above and blob content is immutable once
	// written, so nothing this loop does can invalidate an entry.
	//
	// It exists for the delta chains, not for repeated lookups of the same
	// rid — each candidate is expanded exactly once. Visiting candidates in
	// delta-chain order (deltaChainOrder, above) keeps a chain's working set
	// to the handful of nodes currently in flight; this budget covers
	// however many chains are interleaved at once, not the whole repository.
	// See internal/content.Cache.Expand and crosslinkCacheBytes.
	st := &linkState{
		cache:        content.NewCache(crosslinkCacheBytes),
		avail:        content.NewAvailabilityCache(),
		missingBlobs: make(map[string]struct{}),
	}

	for start := 0; start < len(candidates); start += crosslinkBatchSize {
		select {
		case <-ctx.Done():
			return st.linked, ctx.Err()
		default:
		}
		end := start + crosslinkBatchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		if err := r.WithTx(func(tx *db.Tx) error {
			return linkBatch(ctx, tx, candidates[start:end], st)
		}); err != nil {
			return st.linked, err
		}
	}

	logDeferredCheckins(st.deferredRids, st.missingBlobs, st.linked)

	// Pass 2: Process pending items (wiki backlinks, ticket rebuilds).
	for _, item := range st.pending {
		_ = item // Stubs return nil, nothing to process yet.
	}
	return st.linked, nil
}

// linkBatch links one batch of candidates on tx. ctx is polled once every
// crosslinkCancelCheckStride candidates so a clone deadline can interrupt the
// sweep within a batch, not only at batch boundaries; on cancellation the batch
// rolls back and its links are not counted. Errors from an individual artifact
// abort the whole sweep. A candidate that is not expandable, not a manifest, or
// a deferred checkin is skipped, matching the single-pass behavior this
// batching preserves.
//
// The linked count and pending items are accumulated locally and merged into st
// only once the batch completes, so a rolled-back batch (cancelled or errored)
// contributes nothing to the count st reports.
func linkBatch(ctx context.Context, tx *db.Tx, batch []candidate, st *linkState) error {
	batchLinked := 0
	var batchPending []pendingItem
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
			continue // not a valid manifest, skip
		}
		if d.Type == deck.Checkin && deferCheckin(tx, c, d, st) {
			continue
		}
		p, handled, linkErr := linkArtifact(tx, c.rid, d, st.cache)
		if linkErr != nil {
			return fmt.Errorf("manifest.Crosslink rid=%d type=%d: %w", c.rid, d.Type, linkErr)
		}
		if !handled {
			continue
		}
		batchLinked++
		batchPending = append(batchPending, p...)
	}
	st.linked += batchLinked
	st.pending = append(st.pending, batchPending...)
	return nil
}

// linkArtifact writes the derived rows for one parsed artifact on tx and
// returns any follow-up pending items. handled is false for artifact types the
// sweep does not link (so the caller does not count them), matching the old
// switch's `default: continue`.
func linkArtifact(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, cache *content.Cache) (pending []pendingItem, handled bool, err error) {
	switch d.Type {
	case deck.Checkin:
		return nil, true, crosslinkCheckin(tx, rid, d, cache)
	case deck.Wiki:
		p, e := crosslinkWiki(tx, rid, d)
		return p, true, e
	case deck.Ticket:
		p, e := crosslinkTicket(tx, rid, d)
		return p, true, e
	case deck.Event:
		p, e := crosslinkEvent(tx, rid, d)
		return p, true, e
	case deck.Attachment:
		return nil, true, crosslinkAttachment(tx, rid, d)
	case deck.Cluster:
		return nil, true, CrosslinkCluster(tx, rid, d)
	case deck.ForumPost:
		return nil, true, crosslinkForum(tx, rid, d)
	case deck.Control:
		return nil, true, crosslinkControl(tx, rid, d)
	}
	return nil, false, nil
}

// deferCheckin reports whether a checkin must be held back this sweep because a
// blob it references (F-cards or the B-card baseline) has not arrived locally
// yet, recording it in deferredRids/missingBlobs when so. The manifest blob
// remains durable in 'blob'; skipping event/leaf/plink/mlink for it keeps a
// downstream Checkout.Update walking the manifest's F-cards via
// manifest.ListFiles from hitting `blob not found` mid-traversal.
//
// Surfaced by agent-infra trial #10 under 16-way concurrent fork+merge: a leaf
// Pulled a multi-blob session in which the merge manifest landed before its
// file blobs, the original crosslink ran with insertCheckinMlinks silently
// skipping missing-blob F-cards, and the next Update on that leaf failed. The
// next sync round that delivers the missing blob also triggers another
// Crosslink sweep (HandleSync runs Crosslink whenever filesRecvd > 0); the
// candidate query selects this rid again because no event row was written, and
// the checkin crosslinks completely.
func deferCheckin(tx *db.Tx, c candidate, d *deck.Deck, st *linkState) bool {
	missing := missingCheckinRefs(tx, d, st.avail)
	if len(missing) == 0 {
		return false
	}
	st.deferredRids = append(st.deferredRids, c.rid)
	for _, u := range missing {
		st.missingBlobs[u] = struct{}{}
	}
	slog.Debug("manifest.Crosslink: deferring checkin",
		"rid", c.rid,
		"uuid", c.uuid,
		"missing_count", len(missing),
		"first_missing", missing[0])
	return true
}

// logDeferredCheckins emits the one-line rollup of checkins held back this
// sweep, with missing-blob UUIDs sorted so it is byte-identical across runs
// that defer the same set regardless of map iteration order.
func logDeferredCheckins(deferredRids []libfossil.FslID, missingBlobs map[string]struct{}, linked int) {
	if len(deferredRids) == 0 {
		return
	}
	distinctMissing := make([]string, 0, len(missingBlobs))
	for u := range missingBlobs {
		distinctMissing = append(distinctMissing, u)
	}
	sort.Strings(distinctMissing)
	slog.Info("manifest.Crosslink: deferred checkins awaiting missing blobs",
		"deferred", len(deferredRids),
		"linked", linked,
		"deferred_rids", deferredRids,
		"missing_blob_count", len(distinctMissing),
		"missing_blobs", distinctMissing)
}

// repairLeafTable recomputes leaf from scratch: a checkin is a leaf iff no
// other checkin names it as a parent. Crosslink no longer maintains leaf
// incrementally as each checkin is linked -- inserting the new checkin and
// deleting its parent only produces the right answer when parents are
// always linked before their children, which delta-chain order does not
// guarantee. A full recompute is Fossil's own leaf_rebuild() shape and,
// unlike the incremental version, is correct regardless of visiting order.
func repairLeafTable(q db.Querier) error {
	if q == nil {
		panic("manifest.repairLeafTable: q must not be nil")
	}
	if _, err := q.Exec("DELETE FROM leaf"); err != nil {
		return fmt.Errorf("repairLeafTable clear: %w", err)
	}
	if _, err := q.Exec(`
		INSERT INTO leaf(rid)
		SELECT objid FROM event
		WHERE type='ci' AND objid NOT IN (SELECT pid FROM plink)
	`); err != nil {
		return fmt.Errorf("repairLeafTable insert: %w", err)
	}
	return nil
}

// repairTagPropagation re-runs tag propagation from every self-declared tag
// origin in tagxref, once, now that the whole sweep's plink edges are in
// place.
//
// tag.propagate is mtime-gated: it only overwrites a descendant's copy of a
// tag when the descendant has no declaration of its own and the incoming
// value is newer than whatever is already there, and it walks the plink
// table live at call time rather than a snapshot. That makes replaying
// every origin exactly once, in any order, converge on the same tagxref
// state regardless of which order the origins are replayed in -- an origin
// visited before its descendants exist yet contributes nothing for them,
// but every origin still gets replayed here after every plink edge exists,
// so nothing is lost the way it was when propagation ran once per checkin,
// from the immediate parent only, at the moment each checkin was linked
// (see applyInlineTags and addFWTPlink).
func repairTagPropagation(q db.Querier) error {
	if q == nil {
		panic("manifest.repairTagPropagation: q must not be nil")
	}

	rows, err := q.Query("SELECT DISTINCT rid FROM tagxref WHERE origid = rid")
	if err != nil {
		return fmt.Errorf("repairTagPropagation query: %w", err)
	}
	var origins []libfossil.FslID
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			return fmt.Errorf("repairTagPropagation scan: %w", err)
		}
		origins = append(origins, libfossil.FslID(rid))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("repairTagPropagation rows: %w", err)
	}

	for _, rid := range origins {
		if err := tag.PropagateAll(q, rid); err != nil {
			return fmt.Errorf("repairTagPropagation propagate rid=%d: %w", rid, err)
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

// crosslinkCheckin links one check-in manifest. cache is the whole-sweep
// content cache (nil outside the sweep, e.g. the dephantomize single-artifact
// path); insertCheckinMlinks uses it to expand the parent manifest at most once
// per parent across the sweep.
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
		var parentRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", parentUUID).Scan(&parentRid); err != nil {
			continue // parent blob missing, skip
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

// parentFile is one file's identity in a parent check-in's manifest: the
// content UUID and permission mlink needs to decide whether a child changed the
// file, and to resolve the changed row's pid.
type parentFile struct {
	uuid string
	perm string
}

// insertCheckinMlinks inserts mlink rows for a check-in, emitting a row only
// for a file that CHANGED relative to its primary parent's manifest -- added,
// modified, deleted, or renamed. A file carried over untouched gets no row,
// matching canonical Fossil's add_mlink diff (src/manifest.c) and the rebuild
// path's internal/verify.rebuildMlinks; the prior code emitted one row per
// F-card, over-inserting ~180x on a real repository.
//
// pmid/pid come from the parent MANIFEST (expanded through cache), not the
// parent's mlink rows. The manifest is always present once the check-in is
// eligible to link, whereas the parent's mlink rows are not -- the delta-chain
// visiting order routinely crosslinks a child before its parent -- which is why
// the old parent-mlink lookup defaulted pid to 0 for most rows.
//
// An F-card with an empty UUID is an explicit deletion (delta-manifest
// convention); it is a change, so it still gets a row with fid=0 and pid = the
// parent's blob rid. See insertMlinkRow for the shared parent-column convention.
func insertCheckinMlinks(tx *db.Tx, cache *content.Cache, rid libfossil.FslID, d *deck.Deck) error {
	if tx == nil {
		panic("manifest.insertCheckinMlinks: tx must not be nil")
	}
	if rid <= 0 {
		panic("manifest.insertCheckinMlinks: rid must be positive")
	}
	if d == nil {
		panic("manifest.insertCheckinMlinks: d must not be nil")
	}

	primaryParentMid, mergeParentMids := resolveParentMids(tx, d)
	parentFiles := loadPrimaryParentFiles(tx, cache, primaryParentMid)
	mergeNames := loadMergeParentNames(tx, cache, mergeParentMids)

	// parents carries only the (fnid -> parent blob rid) and merge-fnid entries
	// for the rows actually emitted, so resolveMlinkParent (inside insertMlinkRow)
	// applies Fossil's pid convention against the parent manifest.
	parents := &mlinkParents{
		primaryMid: primaryParentMid,
		primaryFid: make(map[int64]int64),
		mergeFnids: make(map[int64]struct{}),
	}

	for _, f := range d.F {
		pf, inParent := parentFiles[f.Name]
		if mlinkFileUnchanged(f, pf, inParent) {
			continue // carried over untouched: canonical add_mlink emits no row
		}
		fnid, err := ensureFilename(tx, f.Name)
		if err != nil {
			return fmt.Errorf("filename %q: %w", f.Name, err)
		}
		recordMlinkParentOf(tx, parents, fnid, f.Name, pf, inParent, mergeNames)
		var fileRid int64
		if f.UUID != "" {
			if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", f.UUID).Scan(&fileRid); err != nil {
				continue // file blob missing
			}
		}
		if err := insertMlinkRow(tx, rid, fileRid, fnid, f.OldName, f.Perm, parents); err != nil {
			return err
		}
	}
	return nil
}

// mlinkFileUnchanged reports whether F-card f is byte-identical to the primary
// parent's version -- present under the same name with the same content UUID
// and the same mperm. Only then does canonical Fossil's add_mlink skip the row.
// A file absent from the parent (add or rename) or with an empty UUID
// (deletion) is always a change.
func mlinkFileUnchanged(f deck.FileCard, pf parentFile, inParent bool) bool {
	if !inParent {
		return false
	}
	if f.UUID == "" {
		return false
	}
	return f.UUID == pf.uuid && permToMperm(f.Perm) == permToMperm(pf.perm)
}

// recordMlinkParentOf teaches parents how resolveMlinkParent must resolve one
// emitted file's pid: a file carried from the primary parent takes that
// parent's blob rid, a file present only in a merge parent takes -1, and a file
// new to this check-in takes 0 (the default when neither map has its fnid). A
// parent file whose blob is absent leaves pid at 0 -- the change is still
// recorded. See resolveMlinkParent for the convention.
func recordMlinkParentOf(tx *db.Tx, parents *mlinkParents, fnid int64, name string, pf parentFile, inParent bool, mergeNames map[string]struct{}) {
	if inParent {
		var parentFileRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", pf.uuid).Scan(&parentFileRid); err == nil {
			parents.primaryFid[fnid] = parentFileRid
		}
		return
	}
	if _, ok := mergeNames[name]; ok {
		parents.mergeFnids[fnid] = struct{}{}
	}
}

// loadPrimaryParentFiles expands the primary parent check-in's manifest and
// returns its full file tree keyed by name. A primaryParentMid of 0 (no P-card,
// or a parent whose blob has not arrived) yields an empty map, so every child
// file reads as new -- canonical Fossil's "first check-in: all files are adds"
// branch. A parent that cannot be expanded or parsed degrades to the same empty
// map rather than failing the sweep, mirroring internal/verify.buildParentFileMap.
func loadPrimaryParentFiles(q db.Querier, cache *content.Cache, primaryParentMid libfossil.FslID) map[string]parentFile {
	if primaryParentMid <= 0 {
		return map[string]parentFile{}
	}
	data, err := expandManifestBytes(q, cache, primaryParentMid)
	if err != nil {
		return map[string]parentFile{}
	}
	pd, err := deck.Parse(data)
	if err != nil {
		// A parent that does not parse (e.g. a PGP-clearsigned manifest, which
		// deck.Parse does not yet strip) leaves the tree unknown: every child
		// file then reads as new. This preserves the pre-fix behavior for those
		// few check-ins rather than dropping their rows; see the package tests
		// for the parseable-parent path this fix targets.
		return map[string]parentFile{}
	}
	files, err := effectiveManifestFiles(q, cache, pd)
	if err != nil {
		return map[string]parentFile{}
	}
	return files
}

// loadMergeParentNames returns the union of filenames present in any merge
// parent's manifest -- the set that decides pid=-1 (a file added by this
// check-in but already present via a merge). An unexpandable merge parent
// contributes nothing rather than failing the sweep.
func loadMergeParentNames(q db.Querier, cache *content.Cache, mergeParentMids []libfossil.FslID) map[string]struct{} {
	names := make(map[string]struct{})
	for _, mid := range mergeParentMids {
		if mid <= 0 {
			continue
		}
		data, err := expandManifestBytes(q, cache, mid)
		if err != nil {
			continue
		}
		md, err := deck.Parse(data)
		if err != nil {
			continue
		}
		files, err := effectiveManifestFiles(q, cache, md)
		if err != nil {
			continue
		}
		for name := range files {
			names[name] = struct{}{}
		}
	}
	return names
}

// effectiveManifestFiles returns a parsed manifest's full file tree keyed by
// name, expanding a delta manifest (B-card) against its baseline so a delta
// parent contributes its whole tree, not just its own changes. cache backs the
// baseline expansion. Mirrors manifest.ListFiles, but tx-scoped and returning
// only the (uuid, perm) mlink needs.
func effectiveManifestFiles(q db.Querier, cache *content.Cache, d *deck.Deck) (map[string]parentFile, error) {
	files := make(map[string]parentFile, len(d.F))
	if d.B != "" {
		baseRid, ok := content.AvailableByUUID(q, d.B)
		if !ok {
			return nil, fmt.Errorf("baseline %s not found", d.B)
		}
		baseData, err := expandManifestBytes(q, cache, baseRid)
		if err != nil {
			return nil, fmt.Errorf("expand baseline: %w", err)
		}
		baseDeck, err := deck.Parse(baseData)
		if err != nil {
			return nil, fmt.Errorf("parse baseline: %w", err)
		}
		for _, f := range baseDeck.F {
			applyFileCard(files, f)
		}
	}
	for _, f := range d.F {
		applyFileCard(files, f)
	}
	return files, nil
}

// applyFileCard folds one F-card into a file tree: an empty UUID removes the
// name (delta-manifest deletion), any other card sets it.
func applyFileCard(files map[string]parentFile, f deck.FileCard) {
	if f.UUID == "" {
		delete(files, f.Name)
		return
	}
	files[f.Name] = parentFile{uuid: f.UUID, perm: f.Perm}
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
	if len(d.P) > maxMlinkMergeParents {
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

// applyInlineTags records T-cards with UUID="*" (self-referencing tags) as
// tagxref origin rows.
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
		if tc.UUID != "*" {
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
			TargetRID: rid,
			SrcRID:    rid, // inline: checkin is its own source
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
		var targetRID int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", tc.UUID).Scan(&targetRID); err != nil {
			continue // target not found
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
		var parentRid int64
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", parentUUID).Scan(&parentRid); err != nil {
			continue // parent blob missing, skip
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

func crosslinkWiki(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) ([]pendingItem, error) {
	if tx == nil {
		panic("crosslinkWiki: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkWiki: rid must be positive")
	}

	if err := addFWTPlink(tx, rid, d); err != nil {
		return nil, fmt.Errorf("wiki plink: %w", err)
	}

	title := d.L
	if title == "" {
		return nil, fmt.Errorf("wiki manifest missing title (L-card)")
	}

	// Apply wiki-<title> tag with value = content length
	wikiLen := fmt.Sprintf("%d", len(d.W))
	if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
		TargetRID: rid,
		SrcRID:    rid,
		TagName:   fmt.Sprintf("wiki-%s", title),
		TagType:   tag.TagSingleton,
		Value:     wikiLen,
		MTime:     libfossil.TimeToJulian(d.D),
	}); err != nil {
		return nil, fmt.Errorf("wiki tag: %w", err)
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
		return nil, fmt.Errorf("wiki event: %w", err)
	}

	return []pendingItem{{Type: 'w', ID: title}}, nil
}

func crosslinkTicket(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) ([]pendingItem, error) {
	if tx == nil {
		panic("crosslinkTicket: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkTicket: rid must be positive")
	}

	ticketUUID := d.K
	if ticketUUID == "" {
		return nil, fmt.Errorf("ticket manifest missing UUID (K-card)")
	}
	if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
		TargetRID: rid,
		SrcRID:    rid,
		TagName:   fmt.Sprintf("tkt-%s", ticketUUID),
		TagType:   tag.TagSingleton,
		MTime:     libfossil.TimeToJulian(d.D),
	}); err != nil {
		return nil, fmt.Errorf("ticket tag: %w", err)
	}
	if err := updateAttachmentComments(tx, ticketUUID, 't'); err != nil {
		return nil, fmt.Errorf("ticket attachment comments: %w", err)
	}
	return []pendingItem{{Type: 't', ID: ticketUUID}}, nil
}

func crosslinkEvent(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) ([]pendingItem, error) {
	if tx == nil {
		panic("crosslinkEvent: tx must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkEvent: rid must be positive")
	}

	if d.E == nil {
		return nil, fmt.Errorf("event manifest missing E-card")
	}
	if err := addFWTPlink(tx, rid, d); err != nil {
		return nil, fmt.Errorf("event plink: %w", err)
	}
	eventID := d.E.UUID
	tagName := fmt.Sprintf("event-%s", eventID)
	mtime := libfossil.TimeToJulian(d.D)
	if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
		TargetRID: rid,
		SrcRID:    rid,
		TagName:   tagName,
		TagType:   tag.TagSingleton,
		Value:     fmt.Sprintf("%d", len(d.W)),
		MTime:     mtime,
	}); err != nil {
		return nil, fmt.Errorf("event tag: %w", err)
	}

	var tagid int64
	if err := tx.QueryRow("SELECT tagid FROM tag WHERE tagname=?", tagName).Scan(&tagid); err != nil {
		return nil, fmt.Errorf("event tagid: %w", err)
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
			return nil, fmt.Errorf("event insert: %w", err)
		}
	}
	if err := updateAttachmentComments(tx, eventID, 'e'); err != nil {
		return nil, fmt.Errorf("event attachment comments: %w", err)
	}
	return nil, nil
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

	// Resolve thread references
	froot, fprev, firt := resolveForumRefs(tx, rid, d)

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

// resolveForumRefs resolves the thread root, previous, and in-reply-to rids from deck cards.
func resolveForumRefs(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) (froot, fprev, firt libfossil.FslID) {
	if d.G != "" {
		var rootRid int64
		if tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", d.G).Scan(&rootRid) == nil {
			froot = libfossil.FslID(rootRid)
		}
	}
	if froot == 0 {
		froot = rid // self is thread root
	}
	if len(d.P) > 0 {
		var prevRid int64
		if tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", d.P[0]).Scan(&prevRid) == nil {
			fprev = libfossil.FslID(prevRid)
		}
	}
	if d.I != "" {
		var irtRid int64
		if tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", d.I).Scan(&irtRid) == nil {
			firt = libfossil.FslID(irtRid)
		}
	}
	return froot, fprev, firt
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
