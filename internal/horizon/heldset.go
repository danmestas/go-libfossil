// Package horizon computes the held set of draft-fossil-horizon-00: the
// check-ins whose trees a horizon clone keeps the content of.
package horizon

import (
	"context"
	"database/sql"
	"encoding/json"
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

// Place returns the held set of h, naming C by its hash. placed is false
// when the repository does not hold C as a check-in: such a horizon names no
// held set, and each caller decides what that means for it.
func Place(ctx context.Context, q db.Querier, h cone.Horizon) (k []libfossil.FslID, placed bool, err error) {
	if ctx == nil || q == nil {
		panic("horizon.Place: nil context or querier")
	}
	var c libfossil.FslID
	err = q.QueryRow("SELECT rid FROM blob WHERE uuid=?", h.Hash()).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("horizon: C: %w", err)
	}
	k, err = HeldSet(ctx, q, c, h.Rule())
	if errors.Is(err, ErrNotHeld) {
		return nil, false, nil
	}
	return k, err == nil, err
}

func heldSet(ctx context.Context, q db.Querier, c libfossil.FslID, rule cone.HorizonRule, limit int) ([]libfossil.FslID, error) {
	if ctx == nil || q == nil {
		panic("horizon.HeldSet: nil context or querier")
	}
	if c <= 0 {
		panic("horizon.HeldSet: c must be positive")
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
	g, err := loadGraph(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	return g.heldSet(ctx, c, rule, limit)
}

// Graph is the check-in graph a held set is computed over: the held
// check-ins and the plink edges between two of them, as of the store prefix
// it was read at (draft-fossil-horizon-00 §6.2). A Graph is never changed
// once built, so requests may share one.
type Graph struct {
	children map[libfossil.FslID][]libfossil.FslID
	parents  map[libfossil.FslID][]libfossil.FslID
	checkins []libfossil.FslID // the held check-ins, ascending
	held     map[libfossil.FslID]bool
	edges    int
}

// LoadGraph reads the store's check-in graph.
func LoadGraph(ctx context.Context, q db.Querier) (*Graph, error) {
	if ctx == nil || q == nil {
		panic("horizon.LoadGraph: nil context or querier")
	}
	return loadGraph(ctx, q, heldSetNodes)
}

// loadGraph reads every held check-in and every plink edge between two of
// them, failing once either exceeds limit.
func loadGraph(ctx context.Context, q db.Querier, limit int) (*Graph, error) {
	g := &Graph{children: map[libfossil.FslID][]libfossil.FslID{}, parents: map[libfossil.FslID][]libfossil.FslID{},
		held: map[libfossil.FslID]bool{}}
	checkins, err := heldCheckins(q, `SELECT objid FROM event WHERE type='ci'
		AND NOT EXISTS(SELECT 1 FROM phantom WHERE rid=objid) ORDER BY objid LIMIT ?1`, limit)
	if err != nil {
		return nil, err
	}
	g.addCheckins(checkins)
	err = g.addEdges(ctx, q, `SELECT pid, cid FROM plink WHERE `+
		fmt.Sprintf(heldCheckinSQL, "plink.pid")+` AND `+fmt.Sprintf(heldCheckinSQL, "plink.cid")+
		` LIMIT ?1`, limit, nil)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// Extend returns g with what the artifacts rids have changed: those that are
// now held check-ins, and every plink edge between two held check-ins that
// names one of them. rids are the artifacts appended since g was read and
// any it held as phantoms that were filled; g itself is unchanged. An edge
// between two check-ins g already held is never new: plink records a
// check-in's parents when it is crosslinked, which is when it is appended or
// filled.
func (g *Graph) Extend(ctx context.Context, q db.Querier, rids []libfossil.FslID) (*Graph, error) {
	if g == nil || ctx == nil || q == nil {
		panic("horizon.Graph.Extend: nil graph, context or querier")
	}
	if len(rids) == 0 {
		return g, nil
	}
	ids, err := json.Marshal(rids)
	if err != nil {
		return nil, fmt.Errorf("horizon: extending the graph: %w", err)
	}
	out := &Graph{children: maps.Clone(g.children), parents: maps.Clone(g.parents),
		held: maps.Clone(g.held), checkins: g.checkins, edges: g.edges}
	added, err := heldCheckins(q, `SELECT objid FROM event WHERE type='ci'
		AND objid IN (SELECT value FROM json_each(?2))
		AND NOT EXISTS(SELECT 1 FROM phantom WHERE rid=objid) ORDER BY objid LIMIT ?1`,
		heldSetNodes-len(g.checkins), string(ids))
	if err != nil {
		return nil, err
	}
	out.addCheckins(added)
	seen := map[[2]libfossil.FslID]bool{}
	err = out.addEdges(ctx, q, `SELECT pid, cid FROM plink
		WHERE (cid IN (SELECT value FROM json_each(?2)) OR pid IN (SELECT value FROM json_each(?2)))
		AND `+fmt.Sprintf(heldCheckinSQL, "plink.pid")+` AND `+fmt.Sprintf(heldCheckinSQL, "plink.cid")+
		` LIMIT ?1`, heldSetNodes-g.edges, seen, string(ids))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// addCheckins merges held check-ins, ascending, into g.
func (g *Graph) addCheckins(rids []libfossil.FslID) {
	fresh := make([]libfossil.FslID, 0, len(rids))
	for _, rid := range rids {
		if !g.held[rid] {
			g.held[rid] = true
			fresh = append(fresh, rid)
		}
	}
	if len(fresh) > 0 {
		g.checkins = slices.Concat(g.checkins, fresh)
		slices.Sort(g.checkins)
	}
}

// addEdges reads the edges query names, failing once more than limit have
// been read. A slice g may share with the graph it extends is copied before
// it grows. seen, when set, drops an edge read twice.
func (g *Graph) addEdges(ctx context.Context, q db.Querier, query string, limit int,
	seen map[[2]libfossil.FslID]bool, args ...any) error {
	rows, err := q.Query(query, append([]any{limit + 1}, args...)...)
	if err != nil {
		return fmt.Errorf("horizon: reading plink: %w", err)
	}
	defer rows.Close()
	read := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var pid, cid libfossil.FslID
		if err := rows.Scan(&pid, &cid); err != nil {
			return fmt.Errorf("horizon: reading plink: %w", err)
		}
		read++
		if read > limit {
			return fmt.Errorf("%w: more than %d plink edges", ErrBound, heldSetNodes)
		}
		if seen != nil {
			if seen[[2]libfossil.FslID{pid, cid}] {
				continue
			}
			seen[[2]libfossil.FslID{pid, cid}] = true
		}
		g.edges++
		g.children[pid] = append(slices.Clip(g.children[pid]), cid)
		g.parents[cid] = append(slices.Clip(g.parents[cid]), pid)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("horizon: reading plink: %w", err)
	}
	return nil
}

// HeldSet returns the held set K of check-in c under rule over g, as the
// package's HeldSet does over the store g was read from.
func (g *Graph) HeldSet(ctx context.Context, c libfossil.FslID, rule cone.HorizonRule) ([]libfossil.FslID, error) {
	if g == nil || ctx == nil {
		panic("horizon.Graph.HeldSet: nil graph or context")
	}
	if !g.held[c] {
		return nil, fmt.Errorf("%w: rid=%d", ErrNotHeld, c)
	}
	return g.heldSet(ctx, c, rule, heldSetNodes)
}

func (g *Graph) heldSet(ctx context.Context, c libfossil.FslID, rule cone.HorizonRule, limit int) ([]libfossil.FslID, error) {
	if rule != cone.RuleDescendants && rule != cone.RuleLeaves {
		panic("horizon.HeldSet: unknown rule")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w := walk{ctx: ctx, limit: limit}
	k := map[libfossil.FslID]bool{}
	if err := w.run(c, g.children, k, nil); err != nil {
		return nil, err
	}
	if rule == cone.RuleLeaves {
		if err := addLeafMeetings(&w, g, c, k); err != nil {
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

// JoinedAfter reports whether some check-in of k, c's held set under rule
// over g, lies at or below cursor yet is not in the held set of the
// check-ins g held through cursor (draft-fossil-horizon-00 §6.2.6). Under
// the leaves rule a childless check-in appended after cursor can move a
// meeting point deeper, so that older check-ins join K: a cursor from
// before names a position whose classification kept less than this one
// keeps at or below it, and an origin answers it as if the request carried
// none. rid order is the origin's artifact sequence; a phantom filled later
// is the reclassification an origin records when the fill arrives.
func (g *Graph) JoinedAfter(ctx context.Context, c libfossil.FslID, rule cone.HorizonRule,
	k []libfossil.FslID, cursor int64) (bool, error) {
	if g == nil || ctx == nil {
		panic("horizon.Graph.JoinedAfter: nil graph or context")
	}
	if cursor < 0 {
		panic("horizon.Graph.JoinedAfter: negative cursor")
	}
	if len(k) == 0 || int64(k[0]) > cursor {
		return false, nil // nothing of K lies at or below the cursor
	}
	then := map[libfossil.FslID]bool{}
	if int64(c) <= cursor {
		w := walk{ctx: ctx, limit: heldSetNodes, through: libfossil.FslID(cursor)}
		if err := w.run(c, g.children, then, nil); err != nil {
			return false, err
		}
		if rule == cone.RuleLeaves {
			if err := addLeafMeetings(&w, g, c, then); err != nil {
				return false, err
			}
		}
	}
	for _, rid := range k {
		if int64(rid) > cursor {
			break
		}
		if !then[rid] {
			return true, nil
		}
	}
	return false, nil
}

// addLeafMeetings extends k, holding C and its descendants, by the leaves
// rule: the descendants of every check-in where a childless check-in's
// ancestry meets C's. k stays closed under descendants, because each
// meeting point is added with everything descended from it.
func addLeafMeetings(w *walk, g *Graph, c libfossil.FslID, k map[libfossil.FslID]bool) error {
	descendants := maps.Clone(k)
	ancestors := map[libfossil.FslID]bool{}
	if err := w.run(c, g.parents, ancestors, nil); err != nil {
		return err
	}
	var meetings []libfossil.FslID
	for _, leaf := range g.checkins {
		if w.beyond(leaf) {
			break // ascending: every later check-in is beyond too
		}
		if w.hasChild(g, leaf) || descendants[leaf] || ancestors[leaf] {
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

// heldCheckins lists the held check-ins query names, ascending by rid; its
// first argument is the limit, and more than limit rows fail.
func heldCheckins(q db.Querier, query string, limit int, args ...any) ([]libfossil.FslID, error) {
	rows, err := q.Query(query, append([]any{limit + 1}, args...)...)
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
			return nil, fmt.Errorf("%w: more than %d check-ins", ErrBound, heldSetNodes)
		}
		checkins = append(checkins, rid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("horizon: reading check-ins: %w", err)
	}
	return checkins, nil
}

// walk carries one visited-node budget across every walk of a computation.
// A nonzero through confines it to the check-ins at or below that rid: the
// graph as it stood when the store held no later one.
type walk struct {
	ctx     context.Context
	limit   int
	visits  int
	through libfossil.FslID
}

// beyond reports whether rid lies past the walk's bound.
func (w *walk) beyond(rid libfossil.FslID) bool {
	return w.through > 0 && rid > w.through
}

// hasChild reports whether rid has a child within the walk's bound.
func (w *walk) hasChild(g *Graph, rid libfossil.FslID) bool {
	for _, child := range g.children[rid] {
		if !w.beyond(child) {
			return true
		}
	}
	return false
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
			if out[n] || w.beyond(n) {
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
