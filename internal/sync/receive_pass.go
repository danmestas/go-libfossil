package sync

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// receivePass is one verification pass of received content: one reply on
// the client, one request on the server, one round of a clone. Each runs in
// one write transaction (see runPass), as fossil's page_xfer and client_sync
// run each request and each round.
//
// Section 7.7 (SP-7.7-4) has the repository recompute, before the pass
// commits, every artifact stored in it whose complete bytes are available by
// then, and abort the commit on a mismatch. fossil queues each stored rid
// with verify_before_commit and checks the queue in its commit hook. Here the
// pass remembers the deltas it stored dangling, and the store that makes
// one's bytes available checks it at once. A mismatch is a late
// hashMismatchError, which aborts the pass: its transaction rolls back
// everything the pass stored, as the failed commit hook does. A delta stored
// against a held basis, and full content, are verified before storage and
// never enter the pass.
type receivePass struct {
	dangling map[libfossil.FslID]danglingDelta
	// byRoot indexes dangling by the rid whose arrival makes each delta's
	// bytes available: the unavailable end of its basis chain. The chain may
	// run through rows stored in an earlier pass, so the direct basis is not
	// the key. A store checks only the deltas its artifact grounds, not the
	// whole set: a clone round can leave thousands dangling.
	byRoot map[libfossil.FslID][]libfossil.FslID
	// basis holds the full bytes of artifacts this pass stored, and of the
	// chain nodes checkDelta rebuilt, unchecked against their names as
	// checkDelta's basis is (content.NewUncheckedCache), so checking a delta
	// against a basis the pass already holds starts from those bytes. A
	// file's history arrives as one chain, each delta against the one
	// before: rebuilding every basis from the chain's root made a clone
	// quadratic in its depth. Only bytes of rows that outlive their card
	// enter it (see remember).
	basis *content.Cache
}

// receiveBasisBytes bounds a pass's basis cache, as the receive linker's
// expanded-content cache is bounded.
const receiveBasisBytes = 64 << 20

// danglingDelta is a delta this pass stored before its basis was available:
// the name it must hash to, and the rid that grounds it.
type danglingDelta struct {
	uuid string
	root libfossil.FslID
}

func newReceivePass() *receivePass {
	return newReceivePassOver(content.NewUncheckedCache(receiveBasisBytes))
}

// newReceivePassOver is newReceivePass with a basis cache the caller keeps
// across passes. It holds only rows a committed pass stored or read, so the
// caller must clear it when a pass rolls back.
func newReceivePassOver(basis *content.Cache) *receivePass {
	if basis == nil {
		panic("sync.newReceivePassOver: basis must not be nil")
	}
	return &receivePass{
		dangling: make(map[libfossil.FslID]danglingDelta),
		byRoot:   make(map[libfossil.FslID][]libfossil.FslID),
		basis:    basis,
	}
}

// remember keeps rid's full bytes, which hash to its name, for checking the
// deltas the pass receives against it. It is called only once the card that
// stored rid has committed its savepoint: a rolled-back row's rid can be
// handed to the next row stored.
func (p *receivePass) remember(rid libfossil.FslID, full []byte) {
	if rid <= 0 {
		panic("sync.receivePass.remember: rid must be positive")
	}
	if full == nil {
		panic("sync.receivePass.remember: full must not be nil")
	}
	p.basis.Remember(rid, full)
}

// heldReal reports whether uuid already has a real (non-phantom) row, which
// storage leaves as it is.
func heldReal(q db.Querier, uuid string) (bool, error) {
	var size int64
	err := q.QueryRow("SELECT size FROM blob WHERE uuid = ?", uuid).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("state of %s: %w", uuid, err)
	}
	return size >= 0, nil
}

// rootOf finds the rid whose arrival makes a delta on srcRid available: the
// end of srcRid's basis chain. A basis that is itself dangling in this pass
// shares that delta's root. The delta adds a node to the chain, so the walk
// allows one node fewer than content.MaxDeltaChainDepth.
func (p *receivePass) rootOf(q db.Querier, srcRid libfossil.FslID) (libfossil.FslID, error) {
	return chainEnd(q, srcRid, p.dangling, content.MaxDeltaChainDepth-1)
}

// errChainTooDeep is a basis chain longer than a delta stored on it may be
// and still be read: content.Expand stops at content.MaxDeltaChainDepth.
var errChainTooDeep = errors.New("basis chain too deep to read")

// chainEnd follows delta.srcid from start to the row with no delta, which is
// where the chain's bytes begin, visiting at most limit nodes. A rid in memo
// ends the walk at that delta's recorded root. A chain that loops is an
// error; one longer than limit is errChainTooDeep, since nothing could read
// a delta stored on it. Both stop early, so a planted chain cannot hold the
// write lock for a walk of every row it holds.
func chainEnd(q db.Querier, start libfossil.FslID,
	memo map[libfossil.FslID]danglingDelta, limit int) (libfossil.FslID, error) {
	if limit <= 0 {
		panic("sync.chainEnd: limit must be positive")
	}
	seen := make(map[libfossil.FslID]bool)
	cur := start
	for nodes := 1; ; nodes++ {
		if d, ok := memo[cur]; ok {
			return d.root, nil
		}
		seen[cur] = true
		var next libfossil.FslID
		err := q.QueryRow("SELECT srcid FROM delta WHERE rid=?", cur).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return cur, nil
		}
		if err != nil {
			return 0, fmt.Errorf("basis chain of rid=%d: %w", start, err)
		}
		if seen[next] {
			return 0, fmt.Errorf("basis chain of rid=%d loops at rid=%d", start, next)
		}
		if nodes >= limit {
			return 0, errChainTooDeep
		}
		cur = next
	}
}

// track records a delta stored dangling, which must hash to uuid, whose bytes
// become available when root arrives. Deltas waiting on rid itself now wait
// on root, since rid is no more available than its own basis.
func (p *receivePass) track(rid, root libfossil.FslID, uuid string) {
	if rid <= 0 || root <= 0 {
		panic("sync.receivePass.track: rid and root must be positive")
	}
	if uuid == "" {
		panic("sync.receivePass.track: uuid must not be empty")
	}
	p.dangling[rid] = danglingDelta{uuid: uuid, root: root}
	waiting := append(p.byRoot[rid], rid)
	delete(p.byRoot, rid)
	for _, w := range waiting {
		d := p.dangling[w]
		d.root = root
		p.dangling[w] = d
	}
	p.byRoot[root] = append(p.byRoot[root], waiting...)
}

// cascadeItem is a row the cascade visits, with its own bytes (nil when
// they cannot be rebuilt).
type cascadeItem struct {
	rid   libfossil.FslID
	bytes []byte
	depth int // nodes in rid's basis chain, rid and its root included
}

// verifyFrom checks every dangling delta that rid, just made available, now
// grounds. The first that does not give its HASH is returned as a late
// hashMismatchError, which aborts the pass: nothing is undone here, since
// the pass's transaction rolls it all back.
//
// The walk goes down the delta table from rid, applying each row's delta to
// its parent's bytes as it goes, so a chain of n deltas costs n applies, not
// the n(n+1)/2 that rebuilding each from rid would. fossil gets the same
// bound from content_get's cache of every 8th step. It passes through rows
// this pass did not store, which a dangling delta's chain may run through.
func (p *receivePass) verifyFrom(q db.Querier, rid libfossil.FslID) error {
	if q == nil {
		panic("sync.receivePass.verifyFrom: q must not be nil")
	}
	if rid <= 0 {
		panic("sync.receivePass.verifyFrom: rid must be positive")
	}
	deps := p.byRoot[rid]
	if len(deps) == 0 {
		return nil
	}
	delete(p.byRoot, rid)
	onPath, err := pathRows(q, rid, deps)
	if err != nil {
		return err
	}
	// A basis that cannot be rebuilt is applied to nothing: its dependents
	// get no HASH, as when fossil's blob_delta_apply fails on what
	// content_get gave it.
	rootBytes, _ := content.ExpandUnchecked(q, rid)
	rootDepth, err := chainDepth(q, rid)
	if err != nil {
		return err
	}
	stack := []cascadeItem{{rid, rootBytes, rootDepth}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		kids, err := childRows(q, it.rid, onPath)
		if err != nil {
			return err
		}
		for _, kid := range kids {
			// A row deeper than content.MaxDeltaChainDepth can never be
			// read, so it, and every row on it, is not checked against its
			// name: the pass owes nothing for bytes no read can produce.
			if it.depth+1 > content.MaxDeltaChainDepth {
				continue
			}
			kidBytes, ok, err := p.visit(q, kid, it.bytes)
			if err != nil {
				return err
			}
			if !ok {
				return &hashMismatchError{uuid: p.dangling[kid].uuid, late: true}
			}
			stack = append(stack, cascadeItem{kid, kidBytes, it.depth + 1})
		}
	}
	return nil
}

// visit applies row kid's delta to its parent's bytes. For a dangling delta
// it reports whether the result is its HASH, and drops it from the pass when
// it is; any other row always passes.
func (p *receivePass) visit(q db.Querier, kid libfossil.FslID,
	parent []byte) ([]byte, bool, error) {
	var kidBytes []byte
	if parent != nil {
		payload, err := blob.Load(q, kid)
		if err != nil {
			return nil, false, fmt.Errorf("verify received delta rid=%d: %w", kid, err)
		}
		if out, err := delta.Apply(parent, payload); err == nil {
			kidBytes = out
		}
	}
	d, dangling := p.dangling[kid]
	if !dangling {
		return kidBytes, true, nil
	}
	if kidBytes == nil || !hashMatches(d.uuid, kidBytes) {
		return nil, false, nil
	}
	delete(p.dangling, kid)
	return kidBytes, true, nil
}

// pathRows returns the rows on the chains from each of deps up to root, the
// only rows the cascade needs to visit.
func pathRows(q db.Querier, root libfossil.FslID,
	deps []libfossil.FslID) (map[libfossil.FslID]bool, error) {
	on := make(map[libfossil.FslID]bool, len(deps))
	for _, d := range deps {
		cur := d
		// The walk stops at a row already on a path, so it visits each row
		// once however the chains share or loop; and the cascade does not go
		// past content.MaxDeltaChainDepth whatever this marks.
		for cur != root && !on[cur] {
			on[cur] = true
			err := q.QueryRow("SELECT srcid FROM delta WHERE rid=?", cur).Scan(&cur)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("basis chain of rid=%d: %w", d, err)
			}
		}
	}
	return on, nil
}

// chainDepth counts the nodes of rid's basis chain, rid and its root
// included, stopping one past content.MaxDeltaChainDepth: rid is readable,
// so its chain ends within the bound unless the store is damaged.
func chainDepth(q db.Querier, rid libfossil.FslID) (int, error) {
	cur := rid
	for nodes := 1; ; nodes++ {
		if nodes > content.MaxDeltaChainDepth {
			return nodes, nil
		}
		var next libfossil.FslID
		err := q.QueryRow("SELECT srcid FROM delta WHERE rid=?", cur).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return nodes, nil
		}
		if err != nil {
			return 0, fmt.Errorf("basis chain of rid=%d: %w", rid, err)
		}
		cur = next
	}
}

// childRows lists the rows whose delta is against parent and that lie on a
// path to a dangling delta, in rid order; with a nil onPath, every one.
func childRows(q db.Querier, parent libfossil.FslID,
	onPath map[libfossil.FslID]bool) ([]libfossil.FslID, error) {
	rows, err := q.Query("SELECT rid FROM delta WHERE srcid=? ORDER BY rid", parent)
	if err != nil {
		return nil, fmt.Errorf("deltas on rid=%d: %w", parent, err)
	}
	defer rows.Close()
	var kids []libfossil.FslID
	for rows.Next() {
		var kid libfossil.FslID
		if err := rows.Scan(&kid); err != nil {
			return nil, fmt.Errorf("deltas on rid=%d: %w", parent, err)
		}
		if onPath == nil || onPath[kid] {
			kids = append(kids, kid)
		}
	}
	return kids, rows.Err()
}

// available reports whether the artifact named uuid has its bytes, as
// content.AvailableByUUID does, without walking its chain when the pass
// already knows: bytes the pass holds are available, and a delta dangling in
// the pass is not. A clone asks this of every delta's basis, and the walk is
// as deep as the chain.
func (p *receivePass) available(q db.Querier, uuid string) bool {
	if q == nil {
		panic("sync.receivePass.available: q must not be nil")
	}
	rid, held, err := contentRow(q, uuid)
	if err != nil || !held {
		return false
	}
	if p.basis.Holds(rid) {
		return true
	}
	if _, dangling := p.dangling[rid]; dangling {
		return false
	}
	return content.IsAvailable(q, rid)
}

// contentRow finds the row holding uuid's content. A row without content,
// a phantom or a seed seedcheck dropped, is not held, even when the pass's
// basis cache still has the bytes it was read with before the drop.
func contentRow(q db.Querier, uuid string) (libfossil.FslID, bool, error) {
	var rid libfossil.FslID
	var size int64
	err := q.QueryRow("SELECT rid, size FROM blob WHERE uuid=?", uuid).Scan(&rid, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("look up %s: %w", uuid, err)
	}
	return rid, size >= 0, nil
}

// deltaVerdict is what applying a received delta to its basis showed.
type deltaVerdict int

const (
	// deltaUnverifiable: the basis's bytes are not available in this pass.
	deltaUnverifiable deltaVerdict = iota
	// deltaMatch: the delta gives its HASH.
	deltaMatch
	// deltaMismatch: it does not, or does not apply.
	deltaMismatch
)

// checkDelta applies a received delta to its basis and compares the result
// with uuid. Section 7.7 verifies a reconstructable file before storage, as
// xfer_accept_file applies the delta to the source content_get gives it. A
// basis that is not available leaves the delta unverifiable, to be stored
// dangling, as fossil stores it.
//
// The basis is reconstructed without checking it against its own name, as
// content_get does: integrity is the result's hash, not the basis's, so a
// held basis whose bytes are not its name still has the delta checked
// against what it gives. A basis reported available whose bytes cannot be
// rebuilt (bad zlib, or a delta in its chain that does not apply) is a
// mismatch too: fossil 2.29's content_of_blob and content_get keep going on
// the bytes they have, so xfer_accept_file applies the delta to those and
// hname_verify_hash refuses the result.
//
// A delta that does not apply is a mismatch: fossil verifies what
// blob_delta_apply left, which is not the artifact either.
//
// The basis is rebuilt through the pass's cache (basis) from the deepest node
// of its chain the cache holds, so a chain received basis first costs one
// apply per delta. A basis the cache holds is available; a delta dangling in
// this pass is not; any other basis has its chain walked for availability
// first, as before.
func (p *receivePass) checkDelta(q db.Querier, uuid, deltaSrc string,
	payload []byte) (deltaVerdict, []byte, error) {
	if q == nil {
		panic("sync.checkDelta: q must not be nil")
	}
	if uuid == "" || deltaSrc == "" {
		panic("sync.checkDelta: uuid and deltaSrc must not be empty")
	}
	srcRid, held, err := contentRow(q, deltaSrc)
	if err != nil {
		return deltaUnverifiable, nil, err
	}
	if !held {
		return deltaUnverifiable, nil, nil
	}
	if _, dangling := p.dangling[srcRid]; dangling {
		return deltaUnverifiable, nil, nil
	}
	if !p.basis.Holds(srcRid) {
		ok, err := content.CheckAvailable(q, srcRid)
		if err != nil {
			return deltaUnverifiable, nil, err
		}
		if !ok {
			return deltaUnverifiable, nil, nil
		}
	}
	src, err := p.basis.ExpandUnchecked(q, srcRid)
	if err != nil {
		return deltaMismatch, nil, nil
	}
	target, err := delta.Apply(src, payload)
	if err != nil || !hashMatches(uuid, target) {
		return deltaMismatch, nil, nil
	}
	return deltaMatch, target, nil
}

// hashMismatchError is a received artifact whose content is not its name.
// uuid names the artifact at fault, which for a delta caught late is not the
// card being stored when it is found. late marks that case: the commit check
// of Section 7.7 failed, which aborts the whole pass on either end. A
// mismatch found before storage refuses only its own card on the server; a
// client fails the round on either kind (failsRound, Section 10.1).
type hashMismatchError struct {
	uuid string
	late bool
}

func (e *hashMismatchError) Error() string {
	return "received file " + e.uuid + ": " + errHashMismatch.Error()
}

func (e *hashMismatchError) Unwrap() error { return errHashMismatch }

// abortsPass reports whether err ends a pass by rolling back everything it
// stored (Section 7.7's failed commit check), rather than ending only the
// card that returned it.
func abortsPass(err error) bool {
	var mismatch *hashMismatchError
	return errors.As(err, &mismatch) && mismatch.late
}

// failsRound reports whether err is a received file or cfile whose content
// does not match its name, found before storage or late. Section 10.1 makes
// either an explicit fail-fast path for a client: the round that received it
// commits nothing, its processed prefix included, and no further round
// starts. A server still treats the early case as a card error (abortsPass).
func failsRound(err error) bool {
	var mismatch *hashMismatchError
	return errors.As(err, &mismatch)
}

// horizonRefusal is a horizon store's refusal of a pull whose held set it
// does not hold (horizon §6.4), which Sync returns once its push half has
// converged: the draft makes the refusal fatal to the requesting client.
type horizonRefusal string

func (e horizonRefusal) Error() string { return "error: " + string(e) }

// errPassAborted is what a pass's callback returns to roll its transaction
// back once it has recorded why: the server's late mismatch and §8.1 fatal,
// whose reply is already built.
var errPassAborted = errors.New("sync: pass aborted")

// runPass runs fn in the one write transaction of a verification pass, with
// the repository bound to it: every store, cascade and crosslink fn makes
// through that repository commits together or not at all. fn reports two
// errors. A card error that only ends the card loop (cardErr) leaves the
// prefix processed before it to commit, as Section 10.1 has a handler error
// do; runPass returns it as cardErr with a nil err. Any other error, a late
// hash mismatch included, rolls the pass back and is returned as err, except
// errPassAborted, which rolls back and returns nil: its caller already holds
// the reason. A non-nil err therefore means nothing fn did was kept.
func runPass(r *repo.Repo,
	fn func(bound *repo.Repo) (cardErr, err error)) (cardErr, err error) {
	if r == nil {
		panic("sync.runPass: r must not be nil")
	}
	return runPassIn(r.WithBoundTx, fn)
}

// runPassIn is runPass in the transaction begin opens: r.WithBoundTx, or
// r.WithBoundReadTx for a pass that stores nothing.
func runPassIn(begin func(func(bound *repo.Repo) error) error,
	fn func(bound *repo.Repo) (cardErr, err error)) (cardErr, err error) {
	if begin == nil {
		panic("sync.runPassIn: begin must not be nil")
	}
	if fn == nil {
		panic("sync.runPassIn: fn must not be nil")
	}
	err = begin(func(bound *repo.Repo) error {
		var ferr error
		cardErr, ferr = fn(bound)
		if ferr == nil && abortsPass(cardErr) {
			ferr, cardErr = cardErr, nil
		}
		return ferr
	})
	if errors.Is(err, errPassAborted) {
		err = nil
	}
	if err != nil {
		cardErr = nil
	}
	return cardErr, err
}
