package manifest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// AfterDephantomize crosslinks a formerly-phantom blob and any dependents.
// Matches Fossil's after_dephantomize (content.c:389-456).
//
// ctx bounds the graph walk: this cascade runs synchronously mid-round as a
// clone fills phantoms (see sync.storeReceivedFile), and on a large delta/orphan
// graph can run for many multiples of a clone's deadline. A cancelled ctx aborts
// the walk within crosslinkCancelCheckStride steps (see afterDephantomize),
// mirroring the whole-repository sweep's CrosslinkContext contract. Callers with
// no deadline pass context.Background().
func AfterDephantomize(ctx context.Context, r *repo.Repo, rid libfossil.FslID) {
	if r == nil {
		panic("manifest.AfterDephantomize: r must not be nil")
	}
	if rid <= 0 {
		return
	}
	afterDephantomize(ctx, r, rid, true)
}

// afterDephantomize walks the orphan/delta-child graph reachable from rid and
// crosslinks every artifact it finds, returning how that run executed.
//
// The stats are the batching and cache-sharing guarantees made assertable: they
// exist so a test can pin "one commit, not one per artifact" and "the shared
// cache absorbed the repeated parent-manifest expansions" without a package
// hook. AfterDephantomize discards them.
func afterDephantomize(ctx context.Context, r *repo.Repo, rid libfossil.FslID, linkFlag bool) cascadeStats {
	cl := newCascadeLinker(r)
	if cl.walk(ctx, rid, linkFlag) {
		cl.flush(ctx)
	}
	cache := cl.cache.Stats()
	cl.stats.expansions = int(cache.Misses)
	cl.stats.cacheHits = int(cache.Hits)
	logDeferredCheckinSummary("AfterDephantomize", cl.guard, cl.stats.linked)
	return cl.stats
}

// ReceiveLinkStats reports the receive-scoped content work. ExpansionMisses
// counts only durable-delta expansions; supplied full bytes are linked directly
// and therefore do not take a cache miss.
type ReceiveLinkStats struct {
	ExpansionMisses int
	ExpansionHits   int
	Linked          int
}

const (
	receiveSavepoint    = "receive_artifact"
	receiveWaiterLimit  = 8192
	receiveCascadeLimit = 2000
	receiveCacheBytes   = 64 << 20
	receiveSettledLimit = 100_000
)

// ReceiveLinker incrementally derives manifest state while a clone receives
// blobs. It owns one bounded expanded-content cache, one bounded reverse waiter
// index, and one fixed-cap settled RID set through Finalize, when it runs the
// residual candidate sweep and the single combined leaf/tag repair gate.
type ReceiveLinker struct {
	r     *repo.Repo
	state *linkState

	// waiters maps a newly available UUID to the deferred checkins that need it.
	// waitingOn is the reverse index needed to replace a checkin's wait set when
	// another retry discovers a different missing reference.
	waiters     map[string]map[libfossil.FslID]struct{}
	waitingOn   map[libfossil.FslID]map[string]struct{}
	waiterSize  int
	waiterFull  bool
	needsRepair bool
	inFlight    map[libfossil.FslID]struct{}

	// settled keeps terminal receive-session verdicts without consulting
	// derived tables on every delta child. Its fixed cap is a performance
	// optimization, not a correctness boundary: once full, parsed artifacts
	// consult type-appropriate durable replay evidence before derivation.
	settled map[libfossil.FslID]struct{}
}

// NewReceiveLinker creates the receive-scoped linker and its on-demand
// crosslink tables. It is not safe for concurrent use; one clone owns one.
func NewReceiveLinker(r *repo.Repo) (*ReceiveLinker, error) {
	if r == nil {
		panic("manifest.NewReceiveLinker: r must not be nil")
	}
	return newReceiveLinker(r, receiveCacheBytes)
}

// newReceiveLinker creates a linker with an explicit expanded-content budget.
// CrosslinkContext uses the rebuild budget while incremental receive sessions
// use receiveCacheBytes.
func newReceiveLinker(r *repo.Repo, cacheBytes int64) (*ReceiveLinker, error) {
	if r == nil {
		panic("manifest.newReceiveLinker: r must not be nil")
	}
	if cacheBytes <= 0 {
		panic("manifest.newReceiveLinker: cacheBytes must be > 0")
	}
	if err := prepareCrosslinkTables(r); err != nil {
		return nil, err
	}
	return &ReceiveLinker{
		r:         r,
		state:     newLinkState(cacheBytes),
		waiters:   make(map[string]map[libfossil.FslID]struct{}),
		waitingOn: make(map[libfossil.FslID]map[string]struct{}),
		inFlight:  make(map[libfossil.FslID]struct{}),
		settled:   make(map[libfossil.FslID]struct{}),
	}, nil
}

// LinkStored examines and links one blob inside the transaction that stored it.
// verifiedFullBytes is supplied only for already hash-verified full content;
// nil means the durable delta must be expanded through the receive cache.
//
// One accepted blob may make a large delta or deferred-checkin graph eligible,
// so its synchronous receive-time work is bounded. Residual work is durable:
// Finalize retries the waiters and sweeps every still-unlinked candidate.
func (rl *ReceiveLinker) LinkStored(ctx context.Context, tx *db.Tx, rid libfossil.FslID, uuid string, verifiedFullBytes []byte) error {
	if rl == nil {
		panic("manifest.ReceiveLinker.LinkStored: linker must not be nil")
	}
	if tx == nil {
		panic("manifest.ReceiveLinker.LinkStored: tx must not be nil")
	}
	if rid <= 0 {
		panic("manifest.ReceiveLinker.LinkStored: rid must be positive")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// A blob arrived since the previous checkin examination. Availability
	// negatives are deliberately scoped to one accepted blob so a formerly
	// missing UUID cannot keep a waiter deferred after this transaction fills it.
	rl.state.guard.avail = content.NewAvailabilityCache()
	remaining := receiveCascadeLimit
	if err := rl.linkStored(ctx, tx, rid, verifiedFullBytes, &remaining); err != nil {
		return err
	}
	if uuid == "" || remaining == 0 {
		return nil
	}
	return rl.retryWaitersFor(ctx, tx, uuid, &remaining)
}

// Finalize retries the bounded deferred set, sweeps only residual candidates
// with the same content cache, and repairs leaf/tag state exactly once. Its
// return count is only the final-sweep work, not receive-time links.
func (rl *ReceiveLinker) Finalize(ctx context.Context) (int, error) {
	if rl == nil {
		panic("manifest.ReceiveLinker.Finalize: linker must not be nil")
	}
	if err := ctx.Err(); err != nil {
		_, repairErr := repairCrosslinkState(rl.r, rl.state.linked, err)
		return 0, repairErr
	}
	if err := rl.retryAllWaiters(ctx); err != nil {
		// A cancellation or savepoint-control failure stops new derivation, but
		// receive-time batches may already have committed plink/tagxref rows.
		// They still get the same single repair gate before the cause returns.
		_, repairErr := repairCrosslinkState(rl.r, rl.state.linked, err)
		return 0, repairErr
	}
	linked, sweepErr := crosslinkSweepWithState(ctx, rl.r, rl.state)
	if linked > 0 {
		rl.needsRepair = true
	}
	if !rl.needsRepair {
		return linked, sweepErr
	}
	_, repairErr := repairCrosslinkState(rl.r, 1, sweepErr)
	if repairErr == nil {
		rl.needsRepair = false
	}
	return linked, repairErr
}

// Stats returns a snapshot of receive-time linking and bounded-cache work.
func (rl *ReceiveLinker) Stats() ReceiveLinkStats {
	if rl == nil {
		return ReceiveLinkStats{}
	}
	cache := rl.state.cache.Stats()
	return ReceiveLinkStats{
		ExpansionMisses: int(cache.Misses),
		ExpansionHits:   int(cache.Hits),
		Linked:          rl.state.linked,
	}
}

// linkStored examines one durable blob, using supplied full bytes before it
// touches the cache. Link failures roll back only this artifact and remain
// candidates for Finalize; savepoint-control failures return to abort the
// caller's outer storage transaction.
//
// remaining is shared by a top-level receive call and counts only candidates
// that were neither already settled nor already on the active recursion path.
func (rl *ReceiveLinker) linkStored(ctx context.Context, tx *db.Tx, rid libfossil.FslID, verifiedFullBytes []byte, remaining *int) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, settled := rl.settled[rid]; settled {
		return nil
	}
	if _, busy := rl.inFlight[rid]; busy {
		return nil
	}
	if *remaining == 0 {
		return nil
	}
	(*remaining)--
	rl.inFlight[rid] = struct{}{}
	defer delete(rl.inFlight, rid)

	data := verifiedFullBytes
	direct := data != nil
	available := false
	defer func() {
		if direct {
			// Parsing/linking consumes these exact verified bytes first. An
			// over-budget entry is only eligible for eviction afterwards.
			rl.state.cache.Remember(rid, data)
		}
		if err != nil || !available {
			return
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
			return
		}
		if *remaining != 0 {
			err = rl.retryDeltaChildren(ctx, tx, rid, remaining)
		}
	}()
	if !direct {
		data, err = rl.state.cache.Expand(tx, rid)
		if err != nil {
			return nil // not grounded yet; a later source arrival retries it
		}
	}
	available = true

	d, err := deck.Parse(data)
	if err != nil || !isArtifact(d) {
		err = recordNonArtifacts(tx, []libfossil.FslID{rid})
		if err == nil {
			rl.settle(rid)
		}
		return err
	}
	if len(rl.settled) >= receiveSettledLimit {
		replayed, replayErr := parsedArtifactReplayed(tx, rid, d)
		if replayErr != nil {
			return replayErr
		}
		if replayed {
			return nil
		}
	}

	if d.Type == deck.Checkin {
		missing := missingCheckinRefs(tx, d, rl.state.guard.avail)
		if len(missing) != 0 {
			rl.state.guard.recordDeferred(tx, "ReceiveLinker", rid, missing)
			rl.enqueueWaiter(rid, missing)
			return nil
		}
	}

	if _, err := tx.Exec("SAVEPOINT " + receiveSavepoint); err != nil {
		return fmt.Errorf("manifest.ReceiveLinker savepoint rid=%d: %w", rid, err)
	}
	handled, linkErr := linkArtifact(tx, rid, d, rl.state.cache)
	if linkErr != nil {
		if _, err := tx.Exec("ROLLBACK TO " + receiveSavepoint); err != nil {
			return errors.Join(
				fmt.Errorf("manifest.ReceiveLinker crosslink rid=%d: %w", rid, linkErr),
				fmt.Errorf("manifest.ReceiveLinker rollback rid=%d: %w", rid, err),
			)
		}
		if _, err := tx.Exec("RELEASE " + receiveSavepoint); err != nil {
			return errors.Join(
				fmt.Errorf("manifest.ReceiveLinker crosslink rid=%d: %w", rid, linkErr),
				fmt.Errorf("manifest.ReceiveLinker release rid=%d: %w", rid, err),
			)
		}
		slog.Warn("manifest.ReceiveLinker: crosslink failed, blob left un-crosslinked",
			"rid", rid, "type", d.Type, "error", linkErr)
		return nil
	}
	if _, err := tx.Exec("RELEASE " + receiveSavepoint); err != nil {
		return fmt.Errorf("manifest.ReceiveLinker release rid=%d: %w", rid, err)
	}
	if handled {
		rl.state.linked++
		rl.needsRepair = true
		rl.settle(rid)
	}
	return nil
}

func (rl *ReceiveLinker) settle(rid libfossil.FslID) {
	if len(rl.settled) >= receiveSettledLimit {
		return
	}
	rl.settled[rid] = struct{}{}
}

// parsedArtifactReplayed reports whether a parsed artifact has the durable
// output that proves a prior receive attempt committed. Event and Cluster
// artifacts use their tagxref source rows as replay evidence; every other
// handled artifact type uses its event row.
func parsedArtifactReplayed(tx *db.Tx, rid libfossil.FslID, d *deck.Deck) (bool, error) {
	table, column := "event", "objid"
	if d.Type == deck.Event || d.Type == deck.Cluster {
		table, column = "tagxref", "srcid"
	}
	var replayed int
	if err := tx.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM "+table+" WHERE "+column+"=?)",
		rid,
	).Scan(&replayed); err != nil {
		return false, fmt.Errorf("manifest.ReceiveLinker replay rid=%d: %w", rid, err)
	}
	return replayed != 0, nil
}

// retryDeltaChildren follows the durable delta graph from an accepted source.
// Its query is deliberately just the indexed delta source lookup joined to real
// blobs: receive-session settled RIDs avoid repeated work without materializing
// or scanning any derived table for each source. inFlight still breaks cycles,
// and Finalize's candidate sweep remains the durable correctness backstop.
func (rl *ReceiveLinker) retryDeltaChildren(ctx context.Context, tx *db.Tx, srcid libfossil.FslID, remaining *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if *remaining == 0 {
		return nil
	}
	rows, err := tx.Query(`
		SELECT d.rid
		  FROM delta d JOIN blob b ON b.rid=d.rid
		 WHERE d.srcid=?
		   AND b.size>=0
		 ORDER BY d.rid
		 LIMIT ?`, srcid, *remaining)
	if err != nil {
		return fmt.Errorf("manifest.ReceiveLinker delta children rid=%d: %w", srcid, err)
	}
	defer rows.Close()
	children := make([]libfossil.FslID, 0, *remaining)
	for i := 0; rows.Next(); i++ {
		if i%crosslinkCancelCheckStride == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return fmt.Errorf("manifest.ReceiveLinker delta child rid=%d: %w", srcid, err)
		}
		children = append(children, libfossil.FslID(rid))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("manifest.ReceiveLinker delta children rid=%d: %w", srcid, err)
	}
	for i, rid := range children {
		if *remaining == 0 {
			return nil
		}
		if i%crosslinkCancelCheckStride == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := rl.linkStored(ctx, tx, rid, nil, remaining); err != nil {
			return err
		}
	}
	return nil
}

func (rl *ReceiveLinker) enqueueWaiter(rid libfossil.FslID, missing []string) {
	rl.removeWaiter(rid)
	if rl.waiterFull {
		return
	}
	for _, uuid := range missing {
		if rl.waiterSize == receiveWaiterLimit {
			rl.waiterFull = true
			return
		}
		byRID := rl.waiters[uuid]
		if byRID == nil {
			byRID = make(map[libfossil.FslID]struct{})
			rl.waiters[uuid] = byRID
		}
		if _, exists := byRID[rid]; exists {
			continue
		}
		byRID[rid] = struct{}{}
		byUUID := rl.waitingOn[rid]
		if byUUID == nil {
			byUUID = make(map[string]struct{})
			rl.waitingOn[rid] = byUUID
		}
		byUUID[uuid] = struct{}{}
		rl.waiterSize++
	}
}

func (rl *ReceiveLinker) removeWaiter(rid libfossil.FslID) {
	for uuid := range rl.waitingOn[rid] {
		byRID := rl.waiters[uuid]
		delete(byRID, rid)
		rl.waiterSize--
		if len(byRID) == 0 {
			delete(rl.waiters, uuid)
		}
	}
	delete(rl.waitingOn, rid)
}

func (rl *ReceiveLinker) retryWaitersFor(ctx context.Context, tx *db.Tx, uuid string, remaining *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if *remaining == 0 {
		return nil
	}
	byRID := rl.waiters[uuid]
	if len(byRID) == 0 {
		return nil
	}
	rids := make([]libfossil.FslID, 0, len(byRID))
	for rid := range byRID {
		rids = append(rids, rid)
	}
	sort.Slice(rids, func(i, j int) bool { return rids[i] < rids[j] })
	for i, rid := range rids {
		if *remaining == 0 {
			return nil
		}
		if i%crosslinkCancelCheckStride == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		rl.removeWaiter(rid)
		if err := rl.linkStored(ctx, tx, rid, nil, remaining); err != nil {
			return err
		}
	}
	return nil
}

func (rl *ReceiveLinker) retryAllWaiters(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(rl.waitingOn) == 0 {
		return nil
	}
	rids := make([]libfossil.FslID, 0, len(rl.waitingOn))
	for rid := range rl.waitingOn {
		rids = append(rids, rid)
	}
	sort.Slice(rids, func(i, j int) bool { return rids[i] < rids[j] })
	for i, rid := range rids {
		if i%crosslinkCancelCheckStride == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		rl.removeWaiter(rid)
		if err := rl.r.WithTx(func(tx *db.Tx) error {
			remaining := receiveCascadeLimit
			return rl.linkStored(ctx, tx, rid, nil, &remaining)
		}); err != nil {
			return err
		}
	}
	return nil
}

// cascadeStats reports how one cascade executed: how much work it did, how many
// transactions that cost, and how much content expansion the shared cache
// absorbed.
type cascadeStats struct {
	// attempts counts artifacts handed to the linker, duplicates included: an
	// artifact reachable both as an orphan and as a delta child is crosslinked
	// once per visit, as it always was.
	attempts int
	// linked counts artifacts linkArtifact wrote rows for.
	linked int
	// commits counts write transactions this cascade committed. It is the
	// number the batching exists to hold down (issue #172).
	commits int
	// expansions counts content expansions actually materialized, and cacheHits
	// those the cascade's shared cache served instead. A delta base or parent
	// manifest several artifacts need shows up as one expansion and many hits.
	expansions int
	cacheHits  int
}

// cascadeItem is one artifact the walk discovered. baseline is the orphan row's
// baseline rid when the artifact was found as an orphan, and 0 otherwise; it is
// carried only so a failure is logged against the same context it used to be.
type cascadeItem struct {
	rid      libfossil.FslID
	baseline libfossil.FslID
}

// cascadeSavepoint isolates one artifact's writes inside a batch transaction.
// Never nested: the batch loop releases or rolls back before the next artifact.
const cascadeSavepoint = "cascade_artifact"

// cascadeLinker crosslinks the artifacts one phantom-fill cascade discovers,
// with the two savings the whole-repository sweep already takes: a content cache
// shared by every artifact in the cascade, and one commit per crosslinkBatchSize
// artifacts rather than one per artifact. A single phantom fill can unblock
// hundreds or thousands of related artifacts that share delta bases and parent
// manifests, so the per-artifact shape this replaces paid a transaction commit
// and a from-scratch delta-chain expansion for work a sibling had just done
// (issue #172).
//
// It is the cascade's whole crosslink path; the per-artifact helper it replaces
// had no callers outside this file, so nothing else has to keep working the old
// way. Artifact linking itself routes through linkArtifact, the same type switch
// the sweep uses, rather than a third copy of it.
type cascadeLinker struct {
	r     *repo.Repo
	cache *content.Cache

	// guard holds back a Checkin whose referenced blobs are not yet locally
	// available, exactly as the whole-repository sweep's linkState.guard does
	// (see checkinDeferralGuard in crosslink.go). A deferred checkin gets no
	// event/leaf/plink/mlink/tagxref rows from this cascade at all -- that is
	// what lets the sweep's own candidate query, which only reselects rids
	// with no event row, rediscover and complete it once the missing blob
	// arrives. The cascade has no candidate query or round boundary of its
	// own to retry from, so it depends entirely on falling through to the
	// sweep's recovery path; writing so much as the event row here would
	// strand the checkin permanently (issue #180).
	guard *checkinDeferralGuard

	// pending holds discovered artifacts whose writes have not been committed.
	// The walk discovers work as it goes, so there is no candidate slice to cut
	// into batches the way linkCandidatesInOrder does: the buffer flushes when it
	// fills, and once more when the walk finishes.
	pending []cascadeItem

	stats cascadeStats
}

func newCascadeLinker(r *repo.Repo) *cascadeLinker {
	return &cascadeLinker{
		r:     r,
		cache: content.NewCache(crosslinkCacheBytes),
		guard: newCheckinDeferralGuard(),
	}
}

// walk is the work-stack traversal of the orphan/delta-child graph reachable
// from rid. It reports whether it ran to completion; false means ctx was
// cancelled, in which case the buffered batch is deliberately dropped rather
// than flushed. Dropping costs nothing -- the buffer holds discovered rids, not
// finished work -- and flushing it would spend up to a full batch of crosslinks
// after the deadline already fired, which is the overshoot issue #166 closed.
// Committed batches stand; the artifacts still buffered are simply not
// crosslinked, and the next sync's Crosslink sweep re-selects them because they
// have no event row.
func (cl *cascadeLinker) walk(ctx context.Context, rid libfossil.FslID, linkFlag bool) bool {
	// Work stack replaces recursion. Bounded by total blob count in repo.
	type workItem struct {
		rid      libfossil.FslID
		linkFlag bool
	}
	stack := []workItem{{rid: rid, linkFlag: linkFlag}}

	const maxIterations = 1_000_000 // Guard against pathological delta chains.
	iterations := 0

	for len(stack) > 0 {
		// Observe the deadline every crosslinkCancelCheckStride pops, mirroring
		// linkBatch's stride-batched poll in the whole-repository sweep. This
		// cascade runs synchronously mid-round as a clone fills phantoms (see
		// sync.storeReceivedFile), so without this a cancelled clone context
		// could not interrupt it until the walk finished or hit maxIterations --
		// the deadline-blind overshoot of issue #166. On cancellation we return
		// what has been crosslinked so far, exactly as the batch sweep abandons
		// its remaining candidates; nothing pretends the whole cascade completed.
		if iterations%crosslinkCancelCheckStride == 0 {
			select {
			case <-ctx.Done():
				return false
			default:
			}
		}
		iterations++
		if iterations > maxIterations {
			return true // Safety bound exceeded; commit what is buffered.
		}

		// Pop from stack.
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		current := item.rid

		if current <= 0 {
			continue
		}

		if item.linkFlag {
			if !cl.add(ctx, cascadeItem{rid: current}) {
				return false
			}
		}

		// Process orphaned delta manifests whose baseline is this rid.
		orphanRows, err := cl.r.DB().Query("SELECT rid FROM orphan WHERE baseline=?", current)
		if err == nil {
			var orphans []libfossil.FslID
			for orphanRows.Next() {
				var orid int64
				if orphanRows.Scan(&orid) == nil {
					orphans = append(orphans, libfossil.FslID(orid))
				}
			}
			orphanRows.Close()
			for _, orid := range orphans {
				if !cl.add(ctx, cascadeItem{rid: orid, baseline: current}) {
					return false
				}
			}
			if len(orphans) > 0 {
				// Orphans are the one discovery the delta-child query below can
				// observe: an orphan is reached by its baseline, not by the delta
				// graph, so the same rid can also be a delta child of current, and
				// the query filters exactly on the mlink rows crosslinking it
				// writes. Every other buffered artifact was itself discovered by
				// that query -- delta.rid is a primary key, so an artifact has one
				// delta base and is queried for once -- and so cannot come back.
				// Committing here restores the read-after-write the per-artifact
				// transactions gave the query for free, at one commit per
				// orphan-bearing node rather than one per artifact. It happens
				// before the DELETE below, keeping the original order: orphans
				// crosslinked, orphan rows cleared, children queried.
				if !cl.flush(ctx) {
					return false
				}
				// Runs on r.DB() in autocommit, as it always did: no batch
				// transaction is open between flushes, and no crosslink path
				// writes 'orphan', so batching cannot reorder this against the
				// query above.
				if _, err := cl.r.DB().Exec("DELETE FROM orphan WHERE baseline=?", current); err != nil {
					continue
				}
			}
		}

		// Find delta children not yet crosslinked. The mlink filter reads
		// committed rows, so buffering defers when a just-linked artifact starts
		// filtering itself out. That only matters for a cycle in the delta base
		// relation, which fossil does not produce and content.Expand would not
		// survive: such a cycle now spins until the buffer flushes at
		// crosslinkBatchSize rather than dying after two pops. Bounded,
		// idempotent, and still capped by maxIterations.
		childRows, err := cl.r.DB().Query(
			`SELECT rid FROM delta WHERE srcid=? AND NOT EXISTS (SELECT 1 FROM mlink WHERE mid=delta.rid)`, current)
		if err != nil {
			continue
		}
		var children []libfossil.FslID
		for childRows.Next() {
			var crid int64
			if childRows.Scan(&crid) == nil {
				children = append(children, libfossil.FslID(crid))
			}
		}
		childRows.Close()

		// Push all children onto work stack (reverse order for LIFO processing).
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, workItem{rid: children[i], linkFlag: true})
		}
	}
	return true
}

// add buffers one artifact, flushing once the buffer holds a full batch. It
// reports whether the cascade may continue; false means ctx was cancelled
// during the flush.
func (cl *cascadeLinker) add(ctx context.Context, item cascadeItem) bool {
	cl.pending = append(cl.pending, item)
	if len(cl.pending) < crosslinkBatchSize {
		return true
	}
	return cl.flush(ctx)
}

// flush crosslinks the buffered artifacts in one transaction and commits it,
// reporting whether the cascade may continue.
//
// Failure handling matches the per-artifact transactions this replaces. An
// artifact that cannot be expanded, parsed, or linked is logged and skipped
// while the rest of the batch still commits, so one bad blob no longer costs a
// batch of unrelated successful work; and each artifact's writes are wrapped in
// a savepoint, so a link that fails partway leaves no partial
// event/plink/mlink/tagxref behind -- the atomicity its own transaction used to
// give it. A commit that fails loses the batch instead of one artifact, which is
// the one place batching widens the blast radius: crosslink output is derived,
// the blobs stay durable, and the next sync's Crosslink sweep re-selects every
// artifact that still has no event row, exactly as it did for an artifact whose
// own transaction failed.
func (cl *cascadeLinker) flush(ctx context.Context) bool {
	if len(cl.pending) == 0 {
		return true
	}
	batch := cl.pending
	cl.pending = nil

	err := cl.r.WithTx(func(tx *db.Tx) error {
		for i, item := range batch {
			// Poll on the same stride the sweep's linkBatch uses, so a deadline
			// interrupts a batch rather than only the walk between batches. The
			// batch rolls back on cancellation, as linkBatch's does.
			if i%crosslinkCancelCheckStride == 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
			}
			if err := cl.linkOne(tx, item); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		slog.Warn("manifest.AfterDephantomize: crosslink batch failed, blobs left un-crosslinked",
			"artifacts", len(batch), "error", err)
		return true
	}
	cl.stats.commits++
	return true
}

// linkOne crosslinks one artifact on the batch transaction, isolated by a
// savepoint so a mid-artifact failure discards only its own writes.
func (cl *cascadeLinker) linkOne(tx *db.Tx, item cascadeItem) error {
	cl.stats.attempts++
	rid := item.rid

	data, err := cl.cache.Expand(tx, rid)
	if err != nil {
		cl.warn(item, fmt.Errorf("expand rid=%d: %w", rid, err))
		return nil
	}
	d, err := deck.Parse(data)
	if err != nil {
		// Not a manifest, so there is nothing to crosslink -- the ordinary
		// outcome for a file blob, which is most of what a clone delivers.
		// The whole-repository sweep has always treated this as a skip (see
		// linkBatch); warning here instead made a clone log one line per file
		// blob it filled, ~39k of them on fossil's own repository, and buried
		// the failures that do matter (issue #186). A blob that parses but
		// cannot be linked is still a real fault and still warns, below.
		return nil
	}
	if !isArtifact(d) {
		// Satisfies the card grammar but not the artifact grammar -- a file
		// whose leading bytes read as cards, or an artifact missing a card its
		// type requires. Fossil refuses both, so linking either would write
		// rows `fossil rebuild` removes. Same skip the sweep applies in
		// linkBatch; see isArtifact.
		return nil
	}

	// Hold back a Checkin referencing a blob that has not arrived yet, same
	// as the whole-repository sweep's linkBatch does before ever calling
	// linkArtifact. Checked before the savepoint opens, not inside it: a
	// deferred checkin must leave nothing in the open batch transaction at
	// all, matching the sweep's defer skipping every row write. See
	// cascadeLinker.guard and checkinDeferralGuard.
	if d.Type == deck.Checkin && cl.guard.shouldDefer(tx, "AfterDephantomize", rid, d) {
		return nil
	}

	if _, err := tx.Exec("SAVEPOINT " + cascadeSavepoint); err != nil {
		return fmt.Errorf("manifest.AfterDephantomize savepoint rid=%d: %w", rid, err)
	}
	handled, linkErr := linkArtifact(tx, rid, d, cl.cache)
	if linkErr != nil {
		if _, err := tx.Exec("ROLLBACK TO " + cascadeSavepoint); err != nil {
			return fmt.Errorf("manifest.AfterDephantomize rollback rid=%d: %w", rid, err)
		}
		if _, err := tx.Exec("RELEASE " + cascadeSavepoint); err != nil {
			return fmt.Errorf("manifest.AfterDephantomize release rid=%d: %w", rid, err)
		}
		cl.warn(item, fmt.Errorf("link rid=%d type=%d: %w", rid, d.Type, linkErr))
		return nil
	}
	if _, err := tx.Exec("RELEASE " + cascadeSavepoint); err != nil {
		return fmt.Errorf("manifest.AfterDephantomize release rid=%d: %w", rid, err)
	}
	if handled {
		cl.stats.linked++
	}
	return nil
}

// warn logs an artifact-level failure, keeping the orphan and non-orphan
// wordings the per-artifact call sites used so existing log consumers still see
// which side of the walk lost the blob.
func (cl *cascadeLinker) warn(item cascadeItem, err error) {
	if item.baseline != 0 {
		slog.Warn("manifest.AfterDephantomize: crosslink of orphan failed, blob left un-crosslinked",
			"rid", item.rid, "baseline", item.baseline, "error", err)
		return
	}
	slog.Warn("manifest.AfterDephantomize: crosslink failed, blob left un-crosslinked",
		"rid", item.rid, "error", err)
}
