package merge

import (
	"fmt"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// FindCommonAncestor walks the plink DAG backwards from two checkin rids
// to find their most recent common ancestor via bidirectional BFS.
func FindCommonAncestor(r *repo.Repo, ridA, ridB libfossil.FslID) (libfossil.FslID, error) {
	if r == nil {
		panic("merge.FindCommonAncestor: r must not be nil")
	}
	if ridA <= 0 {
		panic("merge.FindCommonAncestor: ridA must be positive")
	}
	if ridB <= 0 {
		panic("merge.FindCommonAncestor: ridB must be positive")
	}
	if ridA == ridB {
		return ridA, nil
	}

	visitedA := map[libfossil.FslID]bool{ridA: true}
	visitedB := map[libfossil.FslID]bool{ridB: true}
	queueA := []libfossil.FslID{ridA}
	queueB := []libfossil.FslID{ridB}

	for len(queueA) > 0 || len(queueB) > 0 {
		if len(queueA) > 0 {
			current := queueA[0]
			queueA = queueA[1:]
			if visitedB[current] {
				return current, nil
			}
			parents, err := getParents(r, current)
			if err != nil {
				return 0, err
			}
			for _, pid := range parents {
				if !visitedA[pid] {
					visitedA[pid] = true
					if visitedB[pid] {
						return pid, nil
					}
					queueA = append(queueA, pid)
				}
			}
		}

		if len(queueB) > 0 {
			current := queueB[0]
			queueB = queueB[1:]
			if visitedA[current] {
				return current, nil
			}
			parents, err := getParents(r, current)
			if err != nil {
				return 0, err
			}
			for _, pid := range parents {
				if !visitedB[pid] {
					visitedB[pid] = true
					if visitedA[pid] {
						return pid, nil
					}
					queueB = append(queueB, pid)
				}
			}
		}
	}

	return 0, fmt.Errorf("no common ancestor for rid %d and %d", ridA, ridB)
}

func getParents(r *repo.Repo, rid libfossil.FslID) ([]libfossil.FslID, error) {
	rows, err := r.DB().Query("SELECT pid FROM plink WHERE cid=?", rid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parents []libfossil.FslID
	for rows.Next() {
		var pid int64
		if err := rows.Scan(&pid); err != nil {
			continue
		}
		parents = append(parents, libfossil.FslID(pid))
	}
	return parents, rows.Err()
}
