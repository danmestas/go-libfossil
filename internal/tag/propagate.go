package tag

import (
	"container/heap"
	"database/sql"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// propagate walks the plink DAG from a target artifact to all descendants,
// inserting/deleting tagxref rows to cascade propagating tags.
// This matches Fossil's tag_propagate in tag.c:34-113.
//
// For propagating tags (type 2), it inserts tagxref rows with srcid=0.
// For cancel tags (type 0), it deletes all tagxref entries for that tag.
// Special case: if tagName is "bgcolor", also updates event.bgcolor.
func propagate(q db.Querier, tagid int64, tagType int, origID libfossil.FslID, mtime float64, value string, tagName string, pid libfossil.FslID) error {
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
		currentRid := item.rid

		if visited[currentRid] {
			continue
		}
		visited[currentRid] = true

		// Query primary children via LEFT JOIN with tagxref.
		// The doit column is 1 if we should propagate/cancel to this child.
		// For propagating: doit = 1 if (srcid=0 AND tagxref.mtime < ?) OR no tagxref entry
		// For cancel: doit = 1 if there's any tagxref entry with srcid=0 (propagated tag)
		var query string
		if tagType == TagPropagating {
			query = `
				SELECT cid, plink.mtime,
				       COALESCE(srcid=0 AND tagxref.mtime < ?, 1) AS doit
				FROM plink
				LEFT JOIN tagxref ON cid=tagxref.rid AND tagxref.tagid=?
				WHERE pid=? AND isprim=1
			`
		} else {
			// For cancel, we want to delete any propagated tag (srcid=0)
			query = `
				SELECT cid, plink.mtime,
				       COALESCE(srcid=0, 0) AS doit
				FROM plink
				LEFT JOIN tagxref ON cid=tagxref.rid AND tagxref.tagid=?
				WHERE pid=? AND isprim=1
			`
		}
		var rows *sql.Rows
		var err error
		if tagType == TagPropagating {
			rows, err = q.Query(query, mtime, tagid, currentRid)
		} else {
			rows, err = q.Query(query, tagid, currentRid)
		}
		if err != nil {
			return fmt.Errorf("query children of %d: %w", currentRid, err)
		}

		children := []struct {
			cid        libfossil.FslID
			childMtime float64
			doit       bool
		}{}

		for rows.Next() {
			var cid int64
			var childMtimeRaw any
			var doitInt int
			if err := rows.Scan(&cid, &childMtimeRaw, &doitInt); err != nil {
				rows.Close()
				return fmt.Errorf("scan child: %w", err)
			}
			childMtime, _ := db.ScanJulianDay(childMtimeRaw)
			children = append(children, struct {
				cid        libfossil.FslID
				childMtime float64
				doit       bool
			}{libfossil.FslID(cid), childMtime, doitInt != 0})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rows error: %w", err)
		}

		// Process each child
		for _, child := range children {
			if child.doit {
				if tagType == TagPropagating {
					// Insert propagating tagxref entry
					if _, err := q.Exec(
						`REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid)
						 VALUES(?, 2, 0, ?, ?, ?, ?)`,
						tagid, origID, value, mtime, child.cid,
					); err != nil {
						return fmt.Errorf("propagate to %d: %w", child.cid, err)
					}

					// Special case: bgcolor updates event table
					if tagName == "bgcolor" {
						if _, err := q.Exec("UPDATE event SET bgcolor=? WHERE objid=?", value, child.cid); err != nil {
							return fmt.Errorf("update event bgcolor for %d: %w", child.cid, err)
						}
					}
				} else {
					// Cancel: delete all tagxref entries for this tag and rid
					if _, err := q.Exec("DELETE FROM tagxref WHERE tagid=? AND rid=?", tagid, child.cid); err != nil {
						return fmt.Errorf("cancel at %d: %w", child.cid, err)
					}

					// Special case: bgcolor cancellation clears event.bgcolor
					if tagName == "bgcolor" {
						if _, err := q.Exec("UPDATE event SET bgcolor=NULL WHERE objid=?", child.cid); err != nil {
							return fmt.Errorf("clear event bgcolor for %d: %w", child.cid, err)
						}
					}
				}

				// Queue child for further processing
				heap.Push(pq, &queueItem{rid: child.cid, mtime: child.childMtime})
			}
		}
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
func PropagateAll(q db.Querier, rid libfossil.FslID) error {
	if q == nil {
		panic("tag.PropagateAll: q must not be nil")
	}
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
		if err := rows.Scan(&e.tagid, &e.tagtype, &mtimeRaw, &e.value, &e.origid); err != nil {
			return fmt.Errorf("tag.PropagateAll scan: %w", err)
		}
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
		if err := propagate(q, e.tagid, tagtype, e.origid, e.mtime, e.value, tagname, rid); err != nil {
			return fmt.Errorf("tag.PropagateAll propagate tagid=%d: %w", e.tagid, err)
		}
	}
	return nil
}
