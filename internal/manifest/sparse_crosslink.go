package manifest

import (
	"context"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
)

func checkinSources(d *deck.Deck) []string {
	sources := append([]string(nil), d.P...)
	for _, q := range d.Q {
		sources = append(sources, q.Target)
		if q.Baseline != "" {
			sources = append(sources, q.Baseline)
		}
	}
	return sources
}

// Parent spines may have metadata without complete file sets. Mlinks must wait
// on the actual tree dependency, not mistake an event row for full derivation.
func (g *checkinDeferralGuard) parentTreeGap(tx *db.Tx, d *deck.Deck) string {
	c, _ := content.StoreCone(tx)
	for _, uuid := range checkinSources(d) {
		rid, ok := content.AvailableByUUID(tx, uuid)
		if !ok {
			if c != nil {
				return uuid
			}
			continue
		}
		data, err := g.cache.Expand(tx, rid)
		if err != nil {
			if c != nil {
				return uuid
			}
			continue
		}
		parent, err := deck.Parse(data)
		if err != nil || parent.Type != deck.Checkin {
			if c != nil {
				return uuid
			}
			continue
		}
		if gap := g.treeGap(tx, parent); gap != "" {
			return gap
		}
	}
	return ""
}

// Partial derivation publishes only spine metadata. The orphan row remains the
// completion marker; no mlink transition is represented by a partial row set.
func crosslinkPartialCheckin(ctx context.Context, tx *db.Tx, rid libfossil.FslID, d *deck.Deck, g *checkinDeferralGuard, leaves *leaf.Pending) (bool, error) {
	c, err := content.StoreCone(tx)
	if err != nil || c == nil {
		return false, err
	}
	if g.coneReferences == nil {
		g.coneReferences, err = content.LoadConeReferences(ctx, tx, c)
		if err != nil {
			return false, err
		}
	}
	ready, err := SparseCheckinReady(ctx, tx, g.cache, d, c, g.coneReferences)
	if err != nil || !ready {
		return false, err
	}
	if err := crosslinkCheckinMetadata(tx, rid, d); err != nil {
		return false, err
	}
	if err := applyInlineTags(tx, rid, d, leaves); err != nil {
		return false, err
	}
	return true, nil
}

// SparseCheckinReady proves cone completeness, not full mlink derivation. The
// caller must load c and refs from the same unchanged snapshot as q; sharing
// refs avoids a whole-store scan for every check-in during verification.
func SparseCheckinReady(ctx context.Context, q db.Querier, cache *content.Cache, d *deck.Deck, c *cone.Cone, refs *content.ConeReferences) (bool, error) {
	if c == nil || refs == nil || d.Type != deck.Checkin || !isArtifact(d) {
		return false, nil
	}
	reader := sparseReadiness{ctx: ctx, q: q, cache: cache, cone: c, refs: refs}
	ready, err := reader.fileSet(d)
	if err != nil || !ready {
		return false, err
	}
	for _, uuid := range checkinSources(d) {
		parent, ok := content.AvailableByUUID(q, uuid)
		if !ok {
			return false, nil
		}
		data, err := cache.Expand(q, parent)
		if err != nil {
			return false, nil
		}
		pd, err := deck.Parse(data)
		if err != nil || pd.Type != deck.Checkin || !isArtifact(pd) {
			return false, nil
		}
		ready, err := reader.fileSet(pd)
		if err != nil || !ready {
			return false, err
		}
	}
	return true, nil
}

type sparseReadiness struct {
	ctx   context.Context
	q     db.Querier
	cache *content.Cache
	cone  *cone.Cone
	refs  *content.ConeReferences
}

// Inspect every held node, including outside ranges, but tolerate only missing
// nodes whose raw references prove them outside across all aliases. The partial
// walk is private readiness evidence, never returned as a complete file set.
func (r sparseReadiness) fileSet(d *deck.Deck) (bool, error) {
	tree := d
	if d.B != "" {
		var err error
		tree, err = loadBaseline(r.q, r.cache, d)
		if err != nil {
			return false, nil
		}
	}
	if isTree(tree) {
		w := &treeWalk{loader: &subtreeLoader{q: r.q, cache: r.cache}, stack: []treeFrame{{cards: tree.S}}}
		w.skipMissing = func(uuid string) bool {
			var node libfossil.FslID
			if err := r.q.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&node); err != nil {
				return false
			}
			return r.refs.Outside(node)
		}
		for len(w.stack) > 0 {
			if err := r.ctx.Err(); err != nil {
				return false, err
			}
			if err := w.step(); err != nil {
				return false, nil
			}
		}
	}
	files, err := checkinFileCardsFiltered(r.q, r.cache, d, r.cone)
	if err != nil {
		return false, nil
	}
	for _, f := range files {
		if r.cone.Covers(f.Name) {
			if _, ok := content.AvailableByUUID(r.q, f.UUID); !ok {
				return false, nil
			}
		}
	}
	return true, nil
}
