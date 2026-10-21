// Package horizon computes the held set of draft-fossil-horizon-00: the
// check-ins whose trees a horizon clone keeps the content of.
package horizon

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

var (
	// ErrNotHeld is returned when C is not a check-in the repository holds:
	// an unknown rid, a phantom, or an artifact of another kind. zit
	// (src/sparse.zig, ClassifyState.extend) reads that case as an empty
	// held set instead; a caller wanting that reading tests for this error.
	ErrNotHeld = errors.New("horizon: C is not a held check-in")
	// ErrBound is returned when the graph or a walk over it exceeds the
	// visited-node bound.
	ErrBound = errors.New("horizon: held-set walk bound exceeded")
)

// heldSetNodes bounds both the plink edges loaded and the check-ins the
// walks visit, as Section 8 requires of the origin's ancestry walk. It is
// the visited-node bound of the repository's other graph walks
// (content.coneReferenceNodes, manifest.maxTreeNodes). A breadth-first
// walk's depth never exceeds its visits, so this bounds depth as well;
// the history of a long-lived repository is far deeper than the tree
// walks' depth bound of 16, so that bound does not transfer.
const heldSetNodes = 1 << 20

// heldCheckinSQL is true for a rid the repository holds as a check-in: a
// phantom has no event row, but an artifact phantomized after crosslinking
// can keep one, so the phantom table is consulted as well.
const heldCheckinSQL = `(EXISTS(SELECT 1 FROM event WHERE objid=%[1]s AND type='ci')
	AND NOT EXISTS(SELECT 1 FROM phantom WHERE rid=%[1]s))`

// HeldSet returns the held set K of Section 3.1 for check-in c under rule,
// ascending by rid. K is computed from the plink and leaf tables as they
// stand, and nothing is stored (Section 3.1: no party stores K).
//
// Edges follow every P card, the primary parent and merge parents alike,
// as zit's walk does (src/sparse.zig, ClassifyState.extend pass 2, at
// horizon-rfc e6ee276c89). Under the descendants rule, a check-in naming a
// member of K as a merge parent is in K.
//
// Under the leaves rule, "the nearest common ancestor of L and C" and "every
// check-in on a path from L back to" it are read as zit's walkParents
// reads them: A is C and all of C's ancestors; L's ancestry is walked
// through every parent, each check-in reached is held, and a check-in in
// A is held but not walked past. So when several common ancestors are
// equally near -- L reaching two parents of a merge C by separate paths --
// every one the walk meets is held, and a leaf whose ancestry never meets
// A is held back to its roots. A leaf already in K or A adds nothing.
//
// Two departures from zit, both toward the draft's text. "Open leaf" is
// fossil's: a row of the leaf table (internal/leaf) with no active closed
// tag. zit takes every check-in with no held child and includes closed
// ones; fossil's table also keeps a branch tip merged into another branch
// as a leaf, so such a branch below C is held back to its fork, and two
// open leaves always have their merge base in K. And phantom parents are
// never held: K is drawn from the check-ins the repository holds.
func HeldSet(ctx context.Context, q db.Querier, c libfossil.FslID, rule cone.HorizonRule) ([]libfossil.FslID, error) {
	return heldSet(ctx, q, c, rule, heldSetNodes)
}

func heldSet(ctx context.Context, q db.Querier, c libfossil.FslID, rule cone.HorizonRule, limit int) ([]libfossil.FslID, error) {
	if ctx == nil || q == nil {
		panic("horizon.HeldSet: nil context or querier")
	}
	if c <= 0 {
		panic("horizon.HeldSet: c must be positive")
	}
	if rule != cone.RuleDescendants && rule != cone.RuleLeaves {
		panic("horizon.HeldSet: unknown rule")
	}
	if limit <= 0 {
		panic("horizon.HeldSet: limit must be positive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var held int
	err := q.QueryRow(`SELECT count(*) WHERE `+fmt.Sprintf(heldCheckinSQL, "?1"), c).Scan(&held)
	if err != nil {
		return nil, fmt.Errorf("horizon: reading C rid=%d: %w", c, err)
	}
	if held == 0 {
		return nil, fmt.Errorf("%w: rid=%d", ErrNotHeld, c)
	}
	g, err := loadGraph(q, limit)
	if err != nil {
		return nil, err
	}
	w := walk{ctx: ctx, limit: limit}
	k := map[libfossil.FslID]bool{}
	if err := w.run(c, g.children, k, nil); err != nil {
		return nil, err
	}
	if rule == cone.RuleLeaves {
		ancestors := map[libfossil.FslID]bool{}
		if err := w.run(c, g.parents, ancestors, nil); err != nil {
			return nil, err
		}
		leaves, err := openLeaves(q, limit)
		if err != nil {
			return nil, err
		}
		for _, leaf := range leaves {
			if k[leaf] || ancestors[leaf] {
				continue
			}
			if err := w.run(leaf, g.parents, k, ancestors); err != nil {
				return nil, err
			}
		}
	}
	out := make([]libfossil.FslID, 0, len(k))
	for rid := range k {
		out = append(out, rid)
	}
	slices.Sort(out)
	return out, nil
}

// graph is the check-in graph as adjacency both ways.
type graph struct {
	children map[libfossil.FslID][]libfossil.FslID
	parents  map[libfossil.FslID][]libfossil.FslID
}

// loadGraph reads every plink edge between two held check-ins in one query,
// failing once more than limit edges have been read.
func loadGraph(q db.Querier, limit int) (graph, error) {
	g := graph{children: map[libfossil.FslID][]libfossil.FslID{}, parents: map[libfossil.FslID][]libfossil.FslID{}}
	rows, err := q.Query(`SELECT pid, cid FROM plink WHERE `+
		fmt.Sprintf(heldCheckinSQL, "plink.pid")+` AND `+fmt.Sprintf(heldCheckinSQL, "plink.cid")+
		` LIMIT ?`, limit+1)
	if err != nil {
		return graph{}, fmt.Errorf("horizon: reading plink: %w", err)
	}
	defer rows.Close()
	edges := 0
	for rows.Next() {
		var pid, cid libfossil.FslID
		if err := rows.Scan(&pid, &cid); err != nil {
			return graph{}, fmt.Errorf("horizon: reading plink: %w", err)
		}
		edges++
		if edges > limit {
			return graph{}, fmt.Errorf("%w: more than %d plink edges", ErrBound, limit)
		}
		g.children[pid] = append(g.children[pid], cid)
		g.parents[cid] = append(g.parents[cid], pid)
	}
	if err := rows.Err(); err != nil {
		return graph{}, fmt.Errorf("horizon: reading plink: %w", err)
	}
	return g, nil
}

// openLeaves lists the open leaves the repository holds, as fossil's leaves
// command does by default: leaf rows without an active closed tag.
func openLeaves(q db.Querier, limit int) ([]libfossil.FslID, error) {
	rows, err := q.Query(`SELECT l.rid FROM leaf l
		WHERE `+fmt.Sprintf(heldCheckinSQL, "l.rid")+`
		  AND NOT EXISTS(SELECT 1 FROM tagxref x JOIN tag t ON t.tagid = x.tagid
		                  WHERE t.tagname = 'closed' AND x.rid = l.rid AND x.tagtype > 0)
		ORDER BY l.rid LIMIT ?`, limit+1)
	if err != nil {
		return nil, fmt.Errorf("horizon: reading leaves: %w", err)
	}
	defer rows.Close()
	var leaves []libfossil.FslID
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return nil, fmt.Errorf("horizon: reading leaves: %w", err)
		}
		if len(leaves) == limit {
			return nil, fmt.Errorf("%w: more than %d open leaves", ErrBound, limit)
		}
		leaves = append(leaves, rid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("horizon: reading leaves: %w", err)
	}
	return leaves, nil
}

// walk carries one visited-node budget across every walk of a computation.
type walk struct {
	ctx    context.Context
	limit  int
	visits int
}

// run adds start and everything reachable from it through next to out,
// breadth first. A rid already in out is not walked again; a rid in stop is
// added but not walked past.
func (w *walk) run(start libfossil.FslID, next map[libfossil.FslID][]libfossil.FslID,
	out, stop map[libfossil.FslID]bool) error {
	if out[start] {
		return nil
	}
	queue := []libfossil.FslID{start}
	if err := w.visit(out, start); err != nil {
		return err
	}
	for len(queue) > 0 {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		cur := queue[0]
		queue = queue[1:]
		if stop[cur] {
			continue
		}
		for _, n := range next[cur] {
			if out[n] {
				continue
			}
			if err := w.visit(out, n); err != nil {
				return err
			}
			queue = append(queue, n)
		}
	}
	return nil
}

func (w *walk) visit(out map[libfossil.FslID]bool, rid libfossil.FslID) error {
	if w.visits >= w.limit {
		return fmt.Errorf("%w: more than %d check-ins visited", ErrBound, w.limit)
	}
	w.visits++
	out[rid] = true
	return nil
}
