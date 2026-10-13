package tag

import (
	"container/heap"
	"database/sql"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
)

// propagation is one tag_propagate walk: the tag being pushed down the
// primary-parent DAG and the schedule its branch-row writes feed.
type propagation struct {
	tagid   int64
	tagType int // TagPropagating or TagCancel; a singleton arrives as a cancel.
	origID  libfossil.FslID
	mtime   float64
	value   string
	tagName string
	// leaves receives a recheck for every child whose branch row the walk
	// writes or deletes (src/tag.c:100-102). Nil only for PropagateAll, whose
	// caller owns the leaf table; see there.
	leaves *leaf.Pending
}

// walkChild is one primary child of a check-in the walk is visiting.
type walkChild struct {
	cid        libfossil.FslID
	childMtime float64
	doit       bool
}

// propagate walks the plink DAG from a target artifact to all descendants,
// inserting/deleting tagxref rows to cascade propagating tags.
// This matches Fossil's tag_propagate in tag.c:34-113.
//
// For propagating tags (type 2), it inserts tagxref rows with srcid=0.
// For cancel tags (type 0), it deletes all tagxref entries for that tag.
// Special case: if tagName is "bgcolor", also updates event.bgcolor.
func (pr propagation) propagate(q db.Querier, pid libfossil.FslID) error {
	if q == nil {
		panic("tag.propagate: q must not be nil")
	}

	// Priority queue seeded with the target artifact (mtime=0.0 for seed)
	pq := &mtimeQueue{}
	heap.Init(pq)
	heap.Push(pq, &queueItem{rid: pid, mtime: 0.0})

	// Guard against DAG cycles or pathologically deep chains.
	// Fossil repos typically have <100K checkins; 1M is a safe upper bound.
	const maxIterations = 1_000_000

	// Visited set prevents re-processing nodes reachable via multiple paths.
	visited := make(map[libfossil.FslID]bool)

	iterations := 0
	for pq.Len() > 0 {
		iterations++
		if iterations > maxIterations {
			return fmt.Errorf("tag.propagate: exceeded %d iterations (possible cycle)", maxIterations)
		}
		item := heap.Pop(pq).(*queueItem)
		if visited[item.rid] {
			continue
		}
		visited[item.rid] = true

		children, err := pr.children(q, item.rid)
		if err != nil {
			return err
		}
		for _, child := range children {
			if !child.doit {
				continue
			}
			if err := pr.apply(q, child.cid); err != nil {
				return err
			}
			heap.Push(pq, &queueItem{rid: child.cid, mtime: child.childMtime})
		}
	}
	return nil
}

// children lists rid's primary children and, for each, whether the walk acts
// on it. doit is whether to propagate the tag to (type 2) or block it on
// (type 0) the child. Both cases carry the same guard as fossil's
// tag_propagate: act on an inherited row (srcid=0) only when it is OLDER than
// this tag (tagxref.mtime < ?); the default when the child has no row for the
// tag is 1 for a propagating tag (give it the row) and 0 for a cancel (nothing
// to remove), bound as a parameter. A newer inherited row is left in place and
// the walk does not descend past it (#288).
func (pr propagation) children(q db.Querier, rid libfossil.FslID) ([]walkChild, error) {
	defaultDoit := 0
	if pr.tagType == TagPropagating {
		defaultDoit = 1
	}
	const query = `
		SELECT cid, plink.mtime,
		       COALESCE(srcid=0 AND tagxref.mtime < ?, ?) AS doit
		FROM plink
		LEFT JOIN tagxref ON cid=tagxref.rid AND tagxref.tagid=?
		WHERE pid=? AND isprim=1
	`
	rows, err := q.Query(query, pr.mtime, defaultDoit, pr.tagid, rid)
	if err != nil {
		return nil, fmt.Errorf("query children of %d: %w", rid, err)
	}
	defer rows.Close()
	var children []walkChild
	for rows.Next() {
		var cid int64
		var childMtimeRaw any
		var doitInt int
		if err := rows.Scan(&cid, &childMtimeRaw, &doitInt); err != nil {
			return nil, fmt.Errorf("scan child: %w", err)
		}
		childMtime, ok := db.ScanTextJulianDay(childMtimeRaw) // plink.mtime
		if !ok {
			return nil, fmt.Errorf("child %d: unreadable plink.mtime %T", cid, childMtimeRaw)
		}
		children = append(children, walkChild{libfossil.FslID(cid), childMtime, doitInt != 0})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return children, nil
}

// apply gives cid the inherited row (a propagating tag) or removes its row (a
// cancel), and schedules cid's leaf recheck when the row is its branch row,
// as tag_propagate does for TAG_BRANCH.
func (pr propagation) apply(q db.Querier, cid libfossil.FslID) error {
	if pr.tagType == TagPropagating {
		if _, err := q.Exec(
			`REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid)
			 VALUES(?, 2, 0, ?, ?, ?, ?)`,
			pr.tagid, pr.origID, nullableValue(pr.value), pr.mtime, cid,
		); err != nil {
			return fmt.Errorf("propagate to %d: %w", cid, err)
		}
		if pr.tagName == "bgcolor" {
			const stmt = "UPDATE event SET bgcolor=? WHERE objid=?"
			if _, err := q.Exec(stmt, pr.value, cid); err != nil {
				return fmt.Errorf("update event bgcolor for %d: %w", cid, err)
			}
		}
	} else {
		const stmt = "DELETE FROM tagxref WHERE tagid=? AND rid=?"
		if _, err := q.Exec(stmt, pr.tagid, cid); err != nil {
			return fmt.Errorf("cancel at %d: %w", cid, err)
		}
		if pr.tagName == "bgcolor" {
			if _, err := q.Exec("UPDATE event SET bgcolor=NULL WHERE objid=?", cid); err != nil {
				return fmt.Errorf("clear event bgcolor for %d: %w", cid, err)
			}
		}
	}
	if pr.tagid != leaf.BranchTagID || pr.leaves == nil {
		return nil
	}
	if err := pr.leaves.Schedule(q, cid); err != nil {
		return fmt.Errorf("schedule leaf recheck of %d: %w", cid, err)
	}
	return nil
}

// queueItem represents a node in the priority queue.
type queueItem struct {
	rid   libfossil.FslID
	mtime float64
	index int // heap index
}

// mtimeQueue implements heap.Interface for mtime-ordered priority queue.
type mtimeQueue []*queueItem

func (pq mtimeQueue) Len() int           { return len(pq) }
func (pq mtimeQueue) Less(i, j int) bool { return pq[i].mtime < pq[j].mtime }
func (pq mtimeQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *mtimeQueue) Push(x any) {
	n := len(*pq)
	item := x.(*queueItem)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *mtimeQueue) Pop() any {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*pq = old[0 : n-1]
	return item
}

// PropagateAll re-propagates all tags from rid to its descendants.
// Matches Fossil's tag_propagate_all (tag.c:118-135).
// Singleton tags (type 1) are treated as cancel (type 0) during propagation.
//
// It schedules no leaf recheck for the branch rows it writes, so it is only
// for a caller that rebuilds the leaf table itself; every write path that
// maintains leaves incrementally uses PropagateAllLeaves.
func PropagateAll(q db.Querier, rid libfossil.FslID) error {
	if q == nil {
		panic("tag.PropagateAll: q must not be nil")
	}
	return propagateAll(q, rid, nil)
}

// PropagateAllLeaves is PropagateAll scheduling, on leaves, a recheck of every
// check-in whose branch row the walk writes or deletes, as fossil's
// tag_propagate_all does through tag_propagate (draft-fossil-repo-state-01
// Section 7.3, deferred scheduling).
func PropagateAllLeaves(q db.Querier, rid libfossil.FslID, leaves *leaf.Pending) error {
	if q == nil {
		panic("tag.PropagateAllLeaves: q must not be nil")
	}
	if leaves == nil {
		panic("tag.PropagateAllLeaves: leaves must not be nil")
	}
	return propagateAll(q, rid, leaves)
}

// propagateAll is tag_propagate_all; leaves is nil only for PropagateAll.
func propagateAll(q db.Querier, rid libfossil.FslID, leaves *leaf.Pending) error {
	if rid <= 0 {
		return nil
	}

	rows, err := q.Query(
		"SELECT tagid, tagtype, mtime, value, origid FROM tagxref WHERE rid=?", rid,
	)
	if err != nil {
		return fmt.Errorf("tag.PropagateAll query: %w", err)
	}
	defer rows.Close()

	type entry struct {
		tagid   int64
		tagtype int
		mtime   float64
		value   string
		origid  libfossil.FslID
	}
	var entries []entry
	for rows.Next() {
		var e entry
		var mtimeRaw any
		var value sql.NullString // tagxref.value is nullable (e.g. CrosslinkCluster's cluster tag)
		if err := rows.Scan(&e.tagid, &e.tagtype, &mtimeRaw, &value, &e.origid); err != nil {
			return fmt.Errorf("tag.PropagateAll scan: %w", err)
		}
		e.value = value.String
		e.mtime, _ = db.ScanJulianDay(mtimeRaw)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tag.PropagateAll rows: %w", err)
	}

	for _, e := range entries {
		tagtype := e.tagtype
		if tagtype == TagSingleton {
			tagtype = TagCancel // Matches Fossil: if(tagtype==1) tagtype=0;
		}
		var tagname string
		if err := q.QueryRow("SELECT tagname FROM tag WHERE tagid=?", e.tagid).Scan(&tagname); err != nil {
			return fmt.Errorf("tag.PropagateAll tagname: %w", err)
		}
		pr := propagation{
			tagid: e.tagid, tagType: tagtype, origID: e.origid, mtime: e.mtime,
			value: e.value, tagName: tagname, leaves: leaves,
		}
		if err := pr.propagate(q, rid); err != nil {
			return fmt.Errorf("tag.PropagateAll propagate tagid=%d: %w", e.tagid, err)
		}
	}
	return nil
}
