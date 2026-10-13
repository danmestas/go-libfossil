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
)

// receivePass is one verification pass of received content: one reply on
// the client, one request on the server, one round of a clone.
//
// Section 7.7 (SP-7.7-4) has the repository recompute, before the pass
// commits, every artifact stored in it whose complete bytes are available by
// then. fossil queues each stored rid with verify_before_commit and checks the
// queue in its commit hook, so a mismatch aborts the whole round. go-libfossil
// commits each card on its own, so there is no pass-wide commit to hook.
// Instead the pass remembers the deltas it stored dangling, and the store
// that makes one's bytes available checks it in that store's own
// transaction. A delta stored against a held basis, and full content, are
// verified before storage and never enter the pass.
type receivePass struct {
	dangling map[libfossil.FslID]danglingDelta
	// byRoot indexes dangling by the rid whose arrival makes each delta's
	// bytes available: the unavailable end of its basis chain. The chain may
	// run through rows stored in an earlier pass, so the direct basis is not
	// the key. A store checks only the deltas its artifact grounds, not the
	// whole set: a clone round can leave thousands dangling.
	byRoot map[libfossil.FslID][]libfossil.FslID
}

// danglingDelta is a delta this pass stored before its basis was available,
// with the state its row had before, which a mismatch restores.
type danglingDelta struct {
	uuid           string
	root           libfossil.FslID
	wasPhantom     bool
	wasPrivate     bool
	wasUnclustered bool
}

func newReceivePass() *receivePass {
	return &receivePass{
		dangling: make(map[libfossil.FslID]danglingDelta),
		byRoot:   make(map[libfossil.FslID][]libfossil.FslID),
	}
}

// priorRow reads the state uuid's row has before a delta is stored under it.
// It reports held when a real (non-phantom) row exists, which storage leaves
// as it is.
func priorRow(q db.Querier, uuid string) (prior danglingDelta, held bool, err error) {
	prior.uuid = uuid
	var size int64
	err = q.QueryRow(`SELECT b.size,
	    EXISTS(SELECT 1 FROM private WHERE rid = b.rid),
	    EXISTS(SELECT 1 FROM unclustered WHERE rid = b.rid)
	  FROM blob b WHERE b.uuid = ?`, uuid).Scan(&size, &prior.wasPrivate,
		&prior.wasUnclustered)
	if errors.Is(err, sql.ErrNoRows) {
		return danglingDelta{uuid: uuid}, false, nil
	}
	if err != nil {
		return prior, false, fmt.Errorf("prior state of %s: %w", uuid, err)
	}
	if size >= 0 {
		return prior, true, nil
	}
	prior.wasPhantom = true
	return prior, false, nil
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

// track records a delta stored dangling whose bytes become available when
// root arrives. Deltas waiting on rid itself now wait on root, since rid is
// no more available than its own basis.
func (p *receivePass) track(rid, root libfossil.FslID, prior danglingDelta) {
	if rid <= 0 || root <= 0 {
		panic("sync.receivePass.track: rid and root must be positive")
	}
	if prior.uuid == "" {
		panic("sync.receivePass.track: prior.uuid must not be empty")
	}
	prior.root = root
	p.dangling[rid] = prior
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
// grounds. A delta that does not give its HASH is returned to the state it
// had before the pass, and the first such HASH is reported as a hash
// mismatch. It runs inside the transaction that stored rid, so the store and
// the reversal commit together.
//
// The walk goes down the delta table from rid, applying each row's delta to
// its parent's bytes as it goes, so a chain of n deltas costs n applies, not
// the n(n+1)/2 that rebuilding each from rid would. fossil gets the same
// bound from content_get's cache of every 8th step. It passes through rows
// this pass did not store, which a dangling delta's chain may run through.
func (p *receivePass) verifyFrom(tx *db.Tx, rid libfossil.FslID) error {
	if tx == nil {
		panic("sync.receivePass.verifyFrom: tx must not be nil")
	}
	if rid <= 0 {
		panic("sync.receivePass.verifyFrom: rid must be positive")
	}
	deps := p.byRoot[rid]
	if len(deps) == 0 {
		return nil
	}
	delete(p.byRoot, rid)
	onPath, err := pathRows(tx, rid, deps)
	if err != nil {
		return err
	}
	// A basis that cannot be rebuilt is applied to nothing: its dependents
	// get no HASH, as when fossil's blob_delta_apply fails on what
	// content_get gave it.
	rootBytes, _ := content.ExpandUnchecked(tx, rid)
	var first error
	stack := []cascadeItem{{rid, rootBytes}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		kids, err := childRows(tx, it.rid, onPath)
		if err != nil {
			return err
		}
		for _, kid := range kids {
			kidBytes, ok, err := p.visit(tx, kid, it.bytes)
			if err != nil {
				return err
			}
			if !ok {
				if first == nil {
					first = &hashMismatchError{uuid: p.dangling[kid].uuid}
				}
				if err := p.revert(tx, kid); err != nil {
					return err
				}
				continue
			}
			stack = append(stack, cascadeItem{kid, kidBytes})
		}
	}
	if err := p.reroot(tx, deps); err != nil {
		return err
	}
	return first
}

// visit applies row kid's delta to its parent's bytes. For a dangling delta
// it reports whether the result is its HASH, and drops it from the pass when
// it is; any other row always passes.
func (p *receivePass) visit(tx *db.Tx, kid libfossil.FslID, parent []byte) ([]byte, bool, error) {
	var kidBytes []byte
	if parent != nil {
		payload, err := blob.Load(tx, kid)
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

// revert returns dangling delta rid to its state before the pass and drops it
// from the pass.
func (p *receivePass) revert(tx *db.Tx, rid libfossil.FslID) error {
	prior := p.dangling[rid]
	delete(p.dangling, rid)
	return revertDangling(tx, rid, prior)
}

// reroot moves the deltas of deps that the cascade did not reach, because a
// delta above them was reverted, to wait on the end of their chain now.
func (p *receivePass) reroot(tx *db.Tx, deps []libfossil.FslID) error {
	for _, d := range deps {
		dd, ok := p.dangling[d]
		if !ok {
			continue
		}
		var src libfossil.FslID
		if err := tx.QueryRow("SELECT srcid FROM delta WHERE rid=?", d).Scan(&src); err != nil {
			return fmt.Errorf("re-root received delta rid=%d: %w", d, err)
		}
		root, err := chainEnd(tx, src, nil)
		if err != nil {
			return err
		}
		dd.root = root
		p.dangling[d] = dd
		p.byRoot[root] = append(p.byRoot[root], d)
	}
	return nil
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

// revertDangling undoes the storage of a delta that turned out not to give
// its HASH, as fossil's aborted commit would: a row that was a phantom is a
// phantom again, with its earlier visibility; a row new in this pass is
// removed, unless something stored since refers to it by rid, in which case
// it stays as a phantom so that reference still resolves.
func revertDangling(tx *db.Tx, rid libfossil.FslID, prior danglingDelta) error {
	referenced, err := ridReferenced(tx, rid)
	if err != nil {
		return err
	}
	stmts := []string{
		"DELETE FROM delta WHERE rid=?",
		"DELETE FROM unclustered WHERE rid=?",
		"DELETE FROM private WHERE rid=?",
	}
	if prior.wasPhantom || referenced {
		stmts = append(stmts,
			"UPDATE blob SET size=-1, content=NULL, rcvid=0 WHERE rid=?",
			"INSERT OR IGNORE INTO phantom(rid) VALUES(?)")
		if prior.wasUnclustered {
			stmts = append(stmts, "INSERT OR IGNORE INTO unclustered(rid) VALUES(?)")
		}
		if prior.wasPrivate {
			stmts = append(stmts, "INSERT OR IGNORE INTO private(rid) VALUES(?)")
		}
	} else {
		stmts = append(stmts, "DELETE FROM phantom WHERE rid=?", "DELETE FROM blob WHERE rid=?")
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt, rid); err != nil {
			return fmt.Errorf("revert received delta rid=%d: %w", rid, err)
		}
	}
	return nil
}

// ridReferenced reports whether a row other than rid's own bookkeeping names
// rid: a delta stored on it, or crosslinked state a clone's linker wrote.
func ridReferenced(q db.Querier, rid libfossil.FslID) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT
	    EXISTS(SELECT 1 FROM delta WHERE srcid=?1)
	 OR EXISTS(SELECT 1 FROM mlink WHERE fid=?1 OR pid=?1 OR mid=?1 OR pmid=?1)
	 OR EXISTS(SELECT 1 FROM plink WHERE pid=?1 OR cid=?1 OR baseid=?1)
	 OR EXISTS(SELECT 1 FROM orphan WHERE baseline=?1)
	 OR EXISTS(SELECT 1 FROM tagxref WHERE rid=?1 OR srcid=?1 OR origid=?1)
	 OR EXISTS(SELECT 1 FROM event WHERE objid=?1)`, rid).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("references to rid=%d: %w", rid, err)
	}
	return n != 0, nil
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
// card being stored when it is found.
type hashMismatchError struct {
	uuid string
}

func (e *hashMismatchError) Error() string {
	return "received file " + e.uuid + ": " + errHashMismatch.Error()
}

func (e *hashMismatchError) Unwrap() error { return errHashMismatch }
