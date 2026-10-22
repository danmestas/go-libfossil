// Package horizon computes the held set of draft-fossil-horizon-00: the
// check-ins whose trees a horizon clone keeps the content of.
package horizon

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
// ascending by rid. K is computed from the plink table as it stands, and
// nothing is stored (Section 3.1: no party stores K).
//
// Both rules are stated over P cards alone, the primary parent and merge
// parents alike, and both yield a set closed under descendants: every child
// of a check-in in K is in K, so leaves and branch tips derive exactly.
// Under the descendants rule K is C and every check-in descended from it.
//
// Under the leaves rule K adds, for every childless check-in L (one no held
// check-in names in a P card, open or closed: not fossil's same-branch
// leaf) that does not descend from C, every descendant of each check-in
// where L's ancestry meets C's. The meeting points are found as zit finds
// them (src/sparse.zig, ClassifyState.extend, at horizon-rfc bbd1435674):
// L's ancestry is walked through every parent, a check-in that is C or one
// of C's ancestors is reached but not walked past, and every such check-in
// reached is a meeting point. L is skipped only when it descends from C,
// not when an earlier meeting point already put it in K: it still has its
// own meeting points. Phantom parents are never held: K is drawn from the
// check-ins the repository holds.
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
		if err := addLeafMeetings(q, &w, g, c, k, limit); err != nil {
			return nil, err
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

// addLeafMeetings extends k, holding C and its descendants, by the leaves
// rule: the descendants of every check-in where a childless check-in's
// ancestry meets C's. k stays closed under descendants, because each
// meeting point is added with everything descended from it.
func addLeafMeetings(q db.Querier, w *walk, g graph, c libfossil.FslID,
	k map[libfossil.FslID]bool, limit int) error {
	descendants := maps.Clone(k)
	ancestors := map[libfossil.FslID]bool{}
	if err := w.run(c, g.parents, ancestors, nil); err != nil {
		return err
	}
	checkins, err := heldCheckins(q, limit)
	if err != nil {
		return err
	}
	var meetings []libfossil.FslID
	for _, leaf := range checkins {
		if len(g.children[leaf]) > 0 || descendants[leaf] || ancestors[leaf] {
			continue
		}
		seen := map[libfossil.FslID]bool{}
		if err := w.run(leaf, g.parents, seen, ancestors); err != nil {
			return err
		}
		for rid := range seen {
			if ancestors[rid] {
				meetings = append(meetings, rid)
			}
		}
	}
	// Sorted so the walk, and the budget it spends, are deterministic.
	slices.Sort(meetings)
	for _, m := range meetings {
		if err := w.run(m, g.children, k, nil); err != nil {
			return err
		}
	}
	return nil
}

// heldCheckins lists the check-ins the repository holds, ascending by rid.
func heldCheckins(q db.Querier, limit int) ([]libfossil.FslID, error) {
	rows, err := q.Query(`SELECT objid FROM event WHERE type='ci'
		AND NOT EXISTS(SELECT 1 FROM phantom WHERE rid=objid)
		ORDER BY objid LIMIT ?`, limit+1)
	if err != nil {
		return nil, fmt.Errorf("horizon: reading check-ins: %w", err)
	}
	defer rows.Close()
	var checkins []libfossil.FslID
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return nil, fmt.Errorf("horizon: reading check-ins: %w", err)
		}
		if len(checkins) == limit {
			return nil, fmt.Errorf("%w: more than %d check-ins", ErrBound, limit)
		}
		checkins = append(checkins, rid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("horizon: reading check-ins: %w", err)
	}
	return checkins, nil
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
