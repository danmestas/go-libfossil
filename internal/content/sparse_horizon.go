package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
)

// horizonCut is what a placed horizon keeps of the store's history: the held
// set K, the blobs K's trees name (above, §4.2), and the manifests,
// baselines and nodes those trees are made of (reach, §5.1), held or not.
type horizonCut struct {
	k     []libfossil.FslID
	above map[string]bool
	reach map[string]bool
}

// horizonTrees computes h's cut (draft-fossil-horizon-00 §4.2, §5.1) from
// the store's own manifests. Nothing is stored: K and these sets are
// recomputed on every classification (§3.1, §3.2). The walk reads K's
// manifests and the baselines and nodes they reach, charging work as it
// goes, under a byte budget scaled to K rather than to the store.
//
// placed is false when C is not a check-in the store holds. Such a horizon
// elides nothing, as zit's classification does (src/sparse.zig,
// ClassifyState.extend, at horizon-rfc e6ee276c89): reading K as empty
// would elide every blob, and after a deepen to a C' not yet pulled the
// pull that brings C' would then ask for none of the blobs it must keep.
// No blob can be proven below a horizon the store cannot place; the cost
// of keeping them wanted is bandwidth, never data.
func horizonTrees(
	ctx context.Context, q db.Querier, h cone.Horizon, limits referenceLimits, work *ReferenceWork,
) (cut horizonCut, placed bool, err error) {
	if ctx == nil || q == nil || work == nil {
		panic("content.horizonTrees: nil context, querier or work")
	}
	k, placed, err := horizon.Place(ctx, q, h)
	if err != nil || !placed {
		return horizonCut{}, false, err
	}
	budget := limits.treeMax
	if scaled := int64(len(k)+1) * limits.treePerCheckin; scaled/limits.treePerCheckin == int64(len(k)+1) && scaled < budget {
		budget = scaled
	}
	walk := treeWalk{ctx: ctx, q: q, budget: budget, work: work,
		cache: NewCache(treeCacheBytes), files: map[string]bool{}, visited: map[string]bool{}}
	above, err := walk.named(k)
	if err != nil {
		return horizonCut{}, false, err
	}
	return horizonCut{k: k, above: above, reach: walk.visited}, true, nil
}

// HorizonReach returns the held set of h and the manifests, baselines and
// nodes its trees reach, by name: what a structural horizon keeps of the
// store's tree structure (draft-fossil-horizon-00 §5.1, §6.2). placed is
// false when the store cannot place C.
func HorizonReach(
	ctx context.Context, q db.Querier, h cone.Horizon,
) (k []libfossil.FslID, reach map[string]bool, placed bool, err error) {
	if ctx == nil || q == nil {
		panic("content.HorizonReach: nil context or querier")
	}
	var work ReferenceWork
	cut, placed, err := horizonTrees(ctx, q, h, defaultReferenceLimits, &work)
	return cut.k, cut.reach, placed, err
}

// HorizonBelow returns the elision predicate of c's content horizon
// (draft-fossil-horizon-00 §4.2) over the artifacts p names, held or not:
// true for a blob a held F card names that no check-in in the held set
// names and that is no structural reference. A synchronization applies it
// to what it received, as LoadAbsences applies it to phantoms. It is nil
// when c has no horizon or the store cannot place C: such a horizon proves
// no blob below it.
func (p *ConeReferences) HorizonBelow(
	ctx context.Context, q db.Querier, c *cone.Cone,
) (func(libfossil.FslID) bool, error) {
	if p == nil || ctx == nil || q == nil || c == nil {
		panic("content.ConeReferences.HorizonBelow: nil references, context, querier or cone")
	}
	h, ok := c.Horizon()
	if !ok {
		return nil, nil
	}
	var work ReferenceWork
	cut, placed, err := horizonTrees(ctx, q, h, defaultReferenceLimits, &work)
	if err != nil || !placed {
		return nil, err
	}
	return func(rid libfossil.FslID) bool { return p.below(cut.above, rid) }, nil
}

// below reports a blob some held F card names that is not above the horizon
// and that structure, which a content horizon holds at every age, does not
// also name.
func (p *ConeReferences) below(above map[string]bool, rid libfossil.FslID) bool {
	return p.file(rid) && !p.structure(rid) && !above[p.names[rid]]
}

// treeCacheBytes bounds the expansions a horizon's walk shares: K's
// manifests are deltas of one another, as are their baselines and nodes.
const treeCacheBytes = 32 << 20

// treeWalk collects every F-card target that a set of check-ins names,
// directly, through their B baselines, or through S sub-manifest nodes.
// mlink cannot answer this: it records a file version only at the check-in
// that changed it, while a check-in's tree names every file it carries.
//
// One visited set over structural names reads a baseline or node shared by
// many check-ins once, so the walk is linear in K's distinct structure, and
// bounded like the repository's other tree walks. A delta manifest's
// deletions and overrides are not applied: a blob its baseline names counts
// as named, as in zit. Keeping more than the exact tree is sound, and it
// keeps this a plain reachability walk. A tree piece the store does not
// hold or cannot read is skipped; it is itself a transit phantom, and the
// classification after it arrives counts the blobs it names.
type treeWalk struct {
	ctx     context.Context
	q       db.Querier
	budget  int64
	work    *ReferenceWork
	cache   *Cache
	files   map[string]bool
	visited map[string]bool
	pending []string
}

func (w *treeWalk) push(uuid string) {
	if uuid != "" && !w.visited[uuid] {
		w.visited[uuid] = true
		w.pending = append(w.pending, uuid)
	}
}

func (w *treeWalk) named(roots []libfossil.FslID) (map[string]bool, error) {
	for _, rid := range roots {
		var uuid string
		if err := w.q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			return nil, fmt.Errorf("horizon: K member rid=%d: %w", rid, err)
		}
		w.push(uuid)
	}
	for len(w.pending) > 0 {
		if err := w.ctx.Err(); err != nil {
			return nil, err
		}
		if len(w.visited) > coneReferenceNodes {
			return nil, fmt.Errorf("horizon: tree walk bound exceeded")
		}
		uuid := w.pending[len(w.pending)-1]
		w.pending = w.pending[:len(w.pending)-1]
		d, err := w.read(uuid)
		if err != nil {
			return nil, err
		}
		for _, f := range d.F {
			if f.UUID != "" {
				w.files[f.UUID] = true
			}
		}
		w.push(d.B)
		for _, s := range d.S {
			w.push(s.UUID)
		}
	}
	return w.files, nil
}

// read returns the check-in or sub-manifest uuid names, or an empty deck for
// a piece the walk skips. Its size is charged before it is expanded.
func (w *treeWalk) read(uuid string) (*deck.Deck, error) {
	var rid libfossil.FslID
	var size int64
	err := w.q.QueryRow("SELECT rid, size FROM blob WHERE uuid=? AND size>=0", uuid).Scan(&rid, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return &deck.Deck{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("horizon: tree piece %s: %w", uuid, err)
	}
	if size > w.budget-w.work.TreeBytes {
		return nil, fmt.Errorf("horizon: tree byte budget exceeded")
	}
	w.work.TreeReads++
	w.work.TreeBytes += size
	data, err := w.cache.Expand(w.q, rid)
	if err != nil || !deck.HasValidZ(data) {
		return &deck.Deck{}, nil
	}
	d, err := deck.Parse(data)
	if err != nil || d.Type != deck.Checkin && d.Type != deck.Submanifest {
		return &deck.Deck{}, nil
	}
	return d, nil
}
