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
	ready, err := partialFileSetReady(ctx, tx, d, g, c)
	if err != nil || !ready {
		return false, err
	}
	for _, uuid := range checkinSources(d) {
		parent, ok := content.AvailableByUUID(tx, uuid)
		if !ok {
			return false, nil
		}
		data, err := g.cache.Expand(tx, parent)
		if err != nil {
			return false, nil
		}
		pd, err := deck.Parse(data)
		if err != nil || pd.Type != deck.Checkin || !isArtifact(pd) {
			return false, nil
		}
		ready, err := partialFileSetReady(ctx, tx, pd, g, c)
		if err != nil || !ready {
			return false, err
		}
	}
	if err := crosslinkCheckinMetadata(tx, rid, d); err != nil {
		return false, err
	}
	if err := applyInlineTags(tx, rid, d, leaves); err != nil {
		return false, err
	}
	return true, nil
}

// Inspect every held node, including outside ranges, but tolerate only missing
// nodes whose raw references prove them outside across all aliases. The partial
// walk is private readiness evidence, never returned as a complete file set.
func partialFileSetReady(ctx context.Context, tx *db.Tx, d *deck.Deck, g *checkinDeferralGuard, c *cone.Cone) (bool, error) {
	tree := d
	if d.B != "" {
		var err error
		tree, err = loadBaseline(tx, g.cache, d)
		if err != nil {
			return false, nil
		}
	}
	if isTree(tree) {
		w := &treeWalk{loader: &subtreeLoader{q: tx, cache: g.cache}, stack: []treeFrame{{cards: tree.S}}}
		w.skipMissing = func(uuid string) bool {
			var node libfossil.FslID
			if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&node); err != nil {
				return false
			}
			return g.coneReferences.Outside(node)
		}
		for len(w.stack) > 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if err := w.step(); err != nil {
				return false, nil
			}
		}
	}
	files, err := checkinFileCardsFiltered(tx, g.cache, d, c)
	if err != nil {
		return false, nil
	}
	for _, f := range files {
		if c.Covers(f.Name) {
			if _, ok := content.AvailableByUUID(tx, f.UUID); !ok {
				return false, nil
			}
		}
	}
	return true, nil
}
