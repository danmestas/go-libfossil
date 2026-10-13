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
}

// danglingDelta is a delta this pass stored before its basis was available:
// the name it must hash to, and the rid that grounds it.
type danglingDelta struct {
	uuid string
	root libfossil.FslID
}

func newReceivePass() *receivePass {
	return &receivePass{
		dangling: make(map[libfossil.FslID]danglingDelta),
		byRoot:   make(map[libfossil.FslID][]libfossil.FslID),
	}
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
// shares that delta's root.
func (p *receivePass) rootOf(q db.Querier, srcRid libfossil.FslID) (libfossil.FslID, error) {
	return chainEnd(q, srcRid, p.dangling)
}

// chainEnd follows delta.srcid from start to the row with no delta, which is
// where the chain's bytes begin. A rid in memo ends the walk at that delta's
// recorded root.
func chainEnd(q db.Querier, start libfossil.FslID,
	memo map[libfossil.FslID]danglingDelta) (libfossil.FslID, error) {
	cur := start
	for steps := 0; ; steps++ {
		if d, ok := memo[cur]; ok {
			return d.root, nil
		}
		var next libfossil.FslID
		err := q.QueryRow("SELECT srcid FROM delta WHERE rid=?", cur).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return cur, nil
		}
		if err != nil {
			return 0, fmt.Errorf("basis chain of rid=%d: %w", start, err)
		}
		if steps > maxChainSteps {
			return 0, fmt.Errorf("basis chain of rid=%d does not end", start)
		}
		cur = next
	}
}

// maxChainSteps bounds a basis-chain walk so a cycle in the delta table is an
// error rather than a hang.
const maxChainSteps = 1 << 24

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
	stack := []cascadeItem{{rid, rootBytes}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		kids, err := childRows(q, it.rid, onPath)
		if err != nil {
			return err
		}
		for _, kid := range kids {
			kidBytes, ok, err := p.visit(q, kid, it.bytes)
			if err != nil {
				return err
			}
			if !ok {
				return &hashMismatchError{uuid: p.dangling[kid].uuid, late: true}
			}
			stack = append(stack, cascadeItem{kid, kidBytes})
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
		for steps := 0; cur != root && !on[cur]; steps++ {
			if steps > maxChainSteps {
				return nil, fmt.Errorf("basis chain of rid=%d does not end", d)
			}
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

// childRows lists the rows whose delta is against parent and that lie on a
// path to a dangling delta, in rid order.
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
		if onPath[kid] {
			kids = append(kids, kid)
		}
	}
	return kids, rows.Err()
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
func checkDelta(q db.Querier, uuid, deltaSrc string, payload []byte) (deltaVerdict, error) {
	if q == nil {
		panic("sync.checkDelta: q must not be nil")
	}
	if uuid == "" || deltaSrc == "" {
		panic("sync.checkDelta: uuid and deltaSrc must not be empty")
	}
	srcRid, ok, err := content.CheckAvailableByUUID(q, deltaSrc)
	if err != nil {
		return deltaUnverifiable, err
	}
	if !ok {
		return deltaUnverifiable, nil
	}
	src, err := content.ExpandUnchecked(q, srcRid)
	if err != nil {
		return deltaMismatch, nil
	}
	target, err := delta.Apply(src, payload)
	if err != nil || !hashMatches(uuid, target) {
		return deltaMismatch, nil
	}
	return deltaMatch, nil
}

// hashMismatchError is a received artifact whose content is not its name.
// uuid names the artifact at fault, which for a delta caught late is not the
// card being stored when it is found. late marks that case: the commit check
// of Section 7.7 failed, which aborts the whole pass, where a mismatch found
// before storage refuses only its own card.
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
	if fn == nil {
		panic("sync.runPass: fn must not be nil")
	}
	err = r.WithBoundTx(func(bound *repo.Repo) error {
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
