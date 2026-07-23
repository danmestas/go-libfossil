package path

import (
	"database/sql"
	"errors"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// PathNode represents a single node in a path through the plink DAG.
type PathNode struct {
	RID   libfossil.FslID
	From  *PathNode
	Depth int
}

// ErrNoPath is returned when no path exists between the two nodes.
var ErrNoPath = errors.New("no path found")

// Shortest finds the shortest path from `from` to `to` in the plink DAG
// using BFS. If directOnly is true, only primary parent edges (isprim=1)
// are traversed. Nodes in the skip set are not visited.
func Shortest(db *sql.DB, from, to libfossil.FslID, directOnly bool, skip map[libfossil.FslID]bool) ([]PathNode, error) {
	if db == nil {
		panic("path.Shortest: db must not be nil")
	}
	if from <= 0 {
		panic("path.Shortest: from must be positive")
	}
	if to <= 0 {
		panic("path.Shortest: to must be positive")
	}
	if from == to {
		return []PathNode{{RID: from, Depth: 0}}, nil
	}

	// Build queries based on directOnly flag.
	fwdQuery := "SELECT cid FROM plink WHERE pid=?"
	revQuery := "SELECT pid FROM plink WHERE cid=?"
	if directOnly {
		fwdQuery += " AND isprim=1"
		revQuery += " AND isprim=1"
	}

	visited := make(map[libfossil.FslID]bool)
	visited[from] = true
	if skip != nil {
		for k := range skip {
			visited[k] = true
		}
	}

	queue := []*PathNode{{RID: from, Depth: 0}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		// Expand neighbors in both directions.
		neighbors, err := neighbors(db, cur.RID, fwdQuery, revQuery)
		if err != nil {
			return nil, fmt.Errorf("path: query neighbors of %d: %w", cur.RID, err)
		}

		for _, nid := range neighbors {
			if visited[nid] {
				continue
			}
			visited[nid] = true
			node := &PathNode{RID: nid, From: cur, Depth: cur.Depth + 1}
			if nid == to {
				return reconstruct(node), nil
			}
			queue = append(queue, node)
		}
	}

	return nil, ErrNoPath
}

// neighbors returns all nodes reachable from rid via forward and reverse edges.
func neighbors(db *sql.DB, rid libfossil.FslID, fwdQuery, revQuery string) ([]libfossil.FslID, error) {
	var result []libfossil.FslID
	for _, q := range []string{fwdQuery, revQuery} {
		rows, err := db.Query(q, rid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id libfossil.FslID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			result = append(result, id)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
	}
	return result, nil
}

// reconstruct walks the From pointers back to the start and returns the
// path in forward order (from -> ... -> to).
func reconstruct(end *PathNode) []PathNode {
	var rev []PathNode
	for n := end; n != nil; n = n.From {
		rev = append(rev, *n)
	}
	// Reverse and fix From pointers (set to nil in result slice).
	out := make([]PathNode, len(rev))
	for i, n := range rev {
		out[len(rev)-1-i] = PathNode{RID: n.RID, Depth: i}
	}
	return out
}
