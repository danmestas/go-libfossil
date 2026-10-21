package path

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// maxChainLength bounds a walk up the primary-parent chain.
const maxChainLength = 1 << 24

// nameChange is one file followed along a path, by filename id: the name it
// had at the start, and the name it has now (0 once deleted).
type nameChange struct {
	orig, cur, next int64
}

// pathEdge is one step of a path between check-ins: the check-in whose
// mlink rows describe it, and whether the step goes from that check-in up
// to its parent (so the rows are read backwards).
type pathEdge struct {
	mid int64
	up  bool
}

// NameChanges is fossil's find_filename_changes (src/path.c) with revOK
// set, as its update calls it: the files renamed between check-ins from and
// to, old name to new name. The path is the one path_shortest finds over
// primary links: up from from through primary parents to the nearest
// ancestor of to, then down through primary children. A rename on the way
// up is taken backwards; a file deleted on the way drops out. With no path
// between them there are no renames.
func NameChanges(q db.Querier, from, to libfossil.FslID) (map[string]string, error) {
	if q == nil {
		panic("path.NameChanges: nil querier")
	}
	if from <= 0 || to <= 0 {
		panic("path.NameChanges: rids must be positive")
	}
	out := map[string]string{}
	if from == to {
		return out, nil
	}
	edges, err := primaryPath(q, int64(from), int64(to))
	if err != nil || edges == nil {
		return out, err
	}
	var all []*nameChange
	for _, e := range edges {
		if all, err = followEdge(q, e, all); err != nil {
			return nil, err
		}
	}
	for _, ch := range all {
		if ch.orig == 0 || ch.cur == 0 {
			continue
		}
		oldName, err := filename(q, ch.orig)
		if err != nil {
			return nil, err
		}
		newName, err := filename(q, ch.cur)
		if err != nil {
			return nil, err
		}
		out[oldName] = newName
	}
	return out, nil
}

// primaryPath returns the edges from check-in from up to the nearest common
// primary ancestor and down to to, or nil when the two share none.
func primaryPath(q db.Querier, from, to int64) ([]pathEdge, error) {
	fromChain, err := primaryChain(q, from)
	if err != nil {
		return nil, err
	}
	toChain, err := primaryChain(q, to)
	if err != nil {
		return nil, err
	}
	onFrom := make(map[int64]int, len(fromChain))
	for i, rid := range fromChain {
		onFrom[rid] = i
	}
	for j, rid := range toChain {
		i, ok := onFrom[rid]
		if !ok {
			continue
		}
		edges := make([]pathEdge, 0, i+j)
		for _, up := range fromChain[:i] {
			edges = append(edges, pathEdge{mid: up, up: true})
		}
		for k := j - 1; k >= 0; k-- {
			edges = append(edges, pathEdge{mid: toChain[k]})
		}
		return edges, nil
	}
	return nil, nil
}

// primaryChain lists rid and its primary ancestors, nearest first.
func primaryChain(q db.Querier, rid int64) ([]int64, error) {
	chain := []int64{rid}
	for len(chain) <= maxChainLength {
		var parent int64
		err := q.QueryRow(
			"SELECT pid FROM plink WHERE cid = ? AND isprim", chain[len(chain)-1],
		).Scan(&parent)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return chain, nil
			}
			return nil, fmt.Errorf("path: parent of %d: %w", chain[len(chain)-1], err)
		}
		chain = append(chain, parent)
	}
	return nil, fmt.Errorf("path: primary chain of %d is longer than %d", rid, maxChainLength)
}

// followEdge applies one edge's renames to the files followed so far, as
// find_filename_changes does for one node of its path: each mlink rename
// (or deletion) moves the file whose current name it starts from, or starts
// following a new one.
func followEdge(q db.Querier, e pathEdge, all []*nameChange) ([]*nameChange, error) {
	rows, err := q.Query(
		"SELECT pfnid, fnid FROM mlink WHERE mid = ? AND (pfnid > 0 OR fid = 0) ORDER BY pfnid",
		e.mid,
	)
	if err != nil {
		return nil, fmt.Errorf("path: renames of %d: %w", e.mid, err)
	}
	defer rows.Close()
	for rows.Next() {
		var pfnid, fnid int64
		if err := rows.Scan(&pfnid, &fnid); err != nil {
			return nil, fmt.Errorf("path: renames of %d: %w", e.mid, err)
		}
		if pfnid == 0 {
			pfnid, fnid = fnid, 0 // a deletion
		}
		if e.up {
			pfnid, fnid = fnid, pfnid
		}
		all = renameOne(all, pfnid, fnid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("path: renames of %d: %w", e.mid, err)
	}
	for _, ch := range all {
		ch.cur = ch.next
	}
	return all, nil
}

// renameOne records that the file now named from becomes to: the newest
// followed file with that current name moves; otherwise, unless the file is
// going away, a new one is followed.
func renameOne(all []*nameChange, from, to int64) []*nameChange {
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].cur == from {
			all[i].next = to
			return all
		}
	}
	if to <= 0 {
		return all
	}
	return append(all, &nameChange{orig: from, cur: from, next: to})
}

func filename(q db.Querier, fnid int64) (string, error) {
	var name string
	if err := q.QueryRow("SELECT name FROM filename WHERE fnid = ?", fnid).Scan(&name); err != nil {
		return "", fmt.Errorf("path: filename %d: %w", fnid, err)
	}
	return name, nil
}
