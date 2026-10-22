package merge

import (
	"context"
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
	// ErrBound is returned by FindCommonAncestorWithin when the walk visits
	// more check-ins than its bound.
	ErrBound = errors.New("merge: common-ancestor walk bound exceeded")
)

// FindCommonAncestor walks the plink DAG backwards from two checkin rids
// to find their most recent common ancestor via bidirectional BFS.
func FindCommonAncestor(r *repo.Repo, ridA, ridB libfossil.FslID) (libfossil.FslID, error) {
	return findCommonAncestor(context.Background(), r, ridA, ridB, math.MaxInt)
}

// FindCommonAncestorWithin is FindCommonAncestor with a visited-node bound,
// for a walk a peer's request starts (draft-fossil-horizon-00 §8): past
// limit check-ins it stops with ErrBound. Both sides' visits count against
// one budget, and a breadth-first walk's depth never exceeds its visits.
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
	visits := 2

	// step expands one check-in of one side's frontier, returning a check-in
	// both sides have reached, or 0.
	step := func(queue *[]libfossil.FslID, mine, theirs map[libfossil.FslID]bool) (libfossil.FslID, error) {
		current := (*queue)[0]
		*queue = (*queue)[1:]
		if theirs[current] {
			return current, nil
		}
		parents, err := getParents(r, current)
		if err != nil {
			return 0, err
		}
		for _, pid := range parents {
			if mine[pid] {
				continue
			}
			if visits >= limit {
				return 0, fmt.Errorf("%w: more than %d check-ins visited", ErrBound, limit)
			}
			visits++
			mine[pid] = true
			if theirs[pid] {
				return pid, nil
			}
			*queue = append(*queue, pid)
		}
		return 0, nil
	}

	for len(queueA) > 0 || len(queueB) > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if len(queueA) > 0 {
			if found, err := step(&queueA, visitedA, visitedB); found != 0 || err != nil {
				return found, err
			}
		}
		if len(queueB) > 0 {
			if found, err := step(&queueB, visitedB, visitedA); found != 0 || err != nil {
				return found, err
			}
		}
	}

	return 0, fmt.Errorf("%w for rid %d and %d", ErrNoCommonAncestor, ridA, ridB)
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
