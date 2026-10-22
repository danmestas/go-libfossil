package merge

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

var (
	// ErrNoCommonAncestor is returned when two check-ins share no ancestor
	// in the plink graph the repository holds.
	ErrNoCommonAncestor = errors.New("no common ancestor")
	// ErrBound is returned by FindCommonAncestorWithin and a bounded
	// FindPivot when the walk examines more check-ins than its bound.
	ErrBound = errors.New("merge: common-ancestor walk bound exceeded")
)

// FindCommonAncestor is the merge base of two check-ins as fossil's
// merge-base command finds it: the pivot (FindPivot) of ridA and ridB. A
// check-in is its own common ancestor.
func FindCommonAncestor(r *repo.Repo, ridA, ridB libfossil.FslID) (libfossil.FslID, error) {
	return findCommonAncestor(context.Background(), r, ridA, ridB, 0)
}

// FindCommonAncestorWithin is FindCommonAncestor with a bound, for a walk a
// peer's request starts (draft-fossil-horizon-00 §8): past limit check-ins
// examined it stops with ErrBound.
func FindCommonAncestorWithin(ctx context.Context, r *repo.Repo, ridA, ridB libfossil.FslID, limit int) (libfossil.FslID, error) {
	if ctx == nil {
		panic("merge.FindCommonAncestorWithin: nil context")
	}
	if limit <= 0 {
		panic("merge.FindCommonAncestorWithin: limit must be positive")
	}
	return findCommonAncestor(ctx, r, ridA, ridB, limit)
}

func findCommonAncestor(ctx context.Context, r *repo.Repo, ridA, ridB libfossil.FslID, limit int) (libfossil.FslID, error) {
	if ridA <= 0 || ridB <= 0 {
		panic("merge.FindCommonAncestor: rids must be positive")
	}
	if ridA == ridB {
		return ridA, nil
	}
	p, err := FindPivot(ctx, r, PivotQuery{Primary: ridA, Secondaries: []libfossil.FslID{ridB}, Limit: limit})
	if err != nil {
		return 0, fmt.Errorf("rid %d and %d: %w", ridA, ridB, err)
	}
	return p.RID, nil
}

// PivotQuery sets up a pivot search as fossil's pivot_set_primary and
// pivot_set_secondary do (pivot.c). A merge makes the merged-in check-in the
// primary and the checkout's version, with every check-in already merged
// into it, the secondaries.
type PivotQuery struct {
	Primary     libfossil.FslID
	Secondaries []libfossil.FslID
	// IgnoreMerges follows primary parent links only, as pivot_find(1)
	// does to find a merge's name pivot.
	IgnoreMerges bool
	// Limit bounds the check-ins examined; past it the search stops with
	// ErrBound. Zero is unbounded.
	Limit int
}

// Pivot is what a pivot search found.
type Pivot struct {
	// RID is the most recent common ancestor of the primary and one of the
	// secondaries, 0 when there is none.
	RID libfossil.FslID
	// Unheld reports that the search reached a check-in the repository does
	// not hold: a phantom, or an artifact with no check-in event, whose
	// unknown ancestry could hold a more recent common ancestor.
	Unheld bool
}

// FindPivot is fossil's pivot_find (pivot.c). It keeps fossil's queue of
// (check-in, side) pairs and repeatedly examines the newest pending
// check-in, ordered as SQLite's ORDER BY pending DESC, mtime DESC walks
// aqueue_idx1: newest event mtime first, then the higher rid, a missing
// event counting as mtime 0.0. A check-in with a child queued from the
// other side is the pivot; otherwise its parents join the queue on each
// side it was reached from, pending again even when examined before (the
// REPLACE of pivot.c). With no pivot it returns ErrNoCommonAncestor, as it
// does when the primary or every secondary is not a check-in, where fossil
// stops with "lack both primary and secondary files".
func FindPivot(ctx context.Context, r *repo.Repo, q PivotQuery) (Pivot, error) {
	if ctx == nil || r == nil {
		panic("merge.FindPivot: nil context or repo")
	}
	if q.Primary <= 0 || len(q.Secondaries) == 0 || q.Limit < 0 {
		panic("merge.FindPivot: needs a primary, a secondary and a non-negative limit")
	}
	limit := q.Limit
	if limit == 0 {
		limit = math.MaxInt
	}
	w := &pivotWalk{db: r.DB(), ignoreMerges: q.IgnoreMerges, queue: pivotQueue{entries: map[pivotKey]pivotEntry{}}}
	if err := w.seed(q); err != nil {
		return Pivot{}, err
	}
	if !w.queue.has(q.Primary, true) || len(w.queue.entries) < 2 {
		return Pivot{Unheld: true}, fmt.Errorf("%w: the primary and a secondary must be check-ins", ErrNoCommonAncestor)
	}
	for examined := 0; examined < limit; examined++ {
		if err := ctx.Err(); err != nil {
			return Pivot{}, err
		}
		rid, ok := w.queue.next()
		if !ok {
			return Pivot{Unheld: w.unheld}, ErrNoCommonAncestor
		}
		found, err := w.isPivot(rid)
		if err != nil || found {
			return Pivot{RID: rid, Unheld: w.unheld}, err
		}
		if err := w.expand(rid); err != nil {
			return Pivot{}, err
		}
	}
	return Pivot{}, fmt.Errorf("%w: more than %d check-ins examined", ErrBound, limit)
}

// pivotWalk is one pivot_find over a repository.
type pivotWalk struct {
	db interface {
		Query(string, ...any) (*sql.Rows, error)
		QueryRow(string, ...any) *sql.Row
	}
	ignoreMerges bool
	queue        pivotQueue
	unheld       bool
}

// seed queues the primary and the secondaries that are check-ins, with
// their event mtimes, as pivot_set_primary and pivot_set_secondary do.
func (w *pivotWalk) seed(q PivotQuery) error {
	for i, rid := range append([]libfossil.FslID{q.Primary}, q.Secondaries...) {
		if rid <= 0 {
			panic("merge.FindPivot: rids must be positive")
		}
		var mtime float64
		err := w.db.QueryRow("SELECT mtime FROM event WHERE objid=? AND type='ci' LIMIT 1", rid).Scan(&mtime)
		if errors.Is(err, sql.ErrNoRows) {
			w.unheld = true
			continue
		}
		if err != nil {
			return fmt.Errorf("merge: pivot: %w", err)
		}
		primary := i == 0
		if !primary && w.queue.has(rid, false) {
			continue // INSERT OR IGNORE
		}
		w.queue.put(pivotKey{rid: rid, primary: primary}, mtime)
	}
	return nil
}

// isPivot reports whether rid has a child queued from a side other than
// one rid was reached from (pivot.c's q2).
func (w *pivotWalk) isPivot(rid libfossil.FslID) (bool, error) {
	query := "SELECT cid FROM plink WHERE pid=?"
	if w.ignoreMerges {
		query += " AND isprim"
	}
	rows, err := w.db.Query(query, rid)
	if err != nil {
		return false, fmt.Errorf("merge: pivot: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid libfossil.FslID
		if err := rows.Scan(&cid); err != nil {
			return false, fmt.Errorf("merge: pivot: %w", err)
		}
		for _, side := range [2]bool{false, true} {
			if w.queue.has(rid, side) && w.queue.has(cid, !side) {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

// expand queues rid's parents on each side rid was reached from and marks
// rid examined (pivot.c's i1, then u1).
func (w *pivotWalk) expand(rid libfossil.FslID) error {
	query := `SELECT pid, coalesce((SELECT mtime FROM event X WHERE X.objid=plink.pid), 0.0),
		EXISTS(SELECT 1 FROM event WHERE objid=plink.pid AND type='ci')
		AND EXISTS(SELECT 1 FROM blob WHERE blob.rid=plink.pid AND size>=0)
		FROM plink WHERE cid=?`
	if w.ignoreMerges {
		query += " AND isprim"
	}
	rows, err := w.db.Query(query, rid)
	if err != nil {
		return fmt.Errorf("merge: pivot: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pid libfossil.FslID
		var mtime float64
		var held bool
		if err := rows.Scan(&pid, &mtime, &held); err != nil {
			return fmt.Errorf("merge: pivot: %w", err)
		}
		w.unheld = w.unheld || !held
		for _, side := range [2]bool{false, true} {
			if w.queue.has(rid, side) {
				w.queue.put(pivotKey{rid: pid, primary: side}, mtime)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("merge: pivot: %w", err)
	}
	w.queue.examined(rid)
	return nil
}

// pivotKey is a row of fossil's aqueue: a check-in and the side, primary or
// secondary, it was reached from.
type pivotKey struct {
	rid     libfossil.FslID
	primary bool
}

type pivotEntry struct {
	mtime   float64
	pending bool
}

// pivotQueue is aqueue: every row ever queued, and a heap of the pending
// ones. A heap item whose row has since been examined, or requeued with
// another mtime, is stale and skipped.
type pivotQueue struct {
	entries map[pivotKey]pivotEntry
	pending pivotHeap
}

func (q *pivotQueue) has(rid libfossil.FslID, primary bool) bool {
	_, ok := q.entries[pivotKey{rid: rid, primary: primary}]
	return ok
}

// put queues k as pending, replacing any row it had.
func (q *pivotQueue) put(k pivotKey, mtime float64) {
	if e, ok := q.entries[k]; ok && e.pending && e.mtime == mtime {
		return
	}
	q.entries[k] = pivotEntry{mtime: mtime, pending: true}
	heap.Push(&q.pending, pivotItem{key: k, mtime: mtime})
}

// next is the check-in of the first pending row in index order.
func (q *pivotQueue) next() (libfossil.FslID, bool) {
	for q.pending.Len() > 0 {
		it := heap.Pop(&q.pending).(pivotItem)
		if e := q.entries[it.key]; e.pending && e.mtime == it.mtime {
			return it.key.rid, true
		}
	}
	return 0, false
}

// examined clears pending on both of rid's rows.
func (q *pivotQueue) examined(rid libfossil.FslID) {
	for _, side := range [2]bool{false, true} {
		k := pivotKey{rid: rid, primary: side}
		if e, ok := q.entries[k]; ok {
			e.pending = false
			q.entries[k] = e
		}
	}
}

type pivotItem struct {
	key   pivotKey
	mtime float64
}

// pivotHeap pops in the order a reverse scan of aqueue_idx1(pending, mtime)
// yields pending rows: mtime, then the primary key (rid, src), descending.
type pivotHeap []pivotItem

func (h pivotHeap) Len() int { return len(h) }
func (h pivotHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if a.mtime != b.mtime {
		return a.mtime > b.mtime
	}
	if a.key.rid != b.key.rid {
		return a.key.rid > b.key.rid
	}
	return a.key.primary && !b.key.primary
}
func (h pivotHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pivotHeap) Push(x any)   { *h = append(*h, x.(pivotItem)) }
func (h *pivotHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}
