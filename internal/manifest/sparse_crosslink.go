package manifest

import (
	"context"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
)

// parentTreeGap prevents a complete child from publishing an incomplete mlink
// transition just because its parent's spine already has an event row.
func (g *checkinDeferralGuard) parentTreeGap(tx *db.Tx, d *deck.Deck) string {
	sources := append([]string(nil), d.P...)
	for _, q := range d.Q {
		sources = append(sources, q.Target)
		if q.Baseline != "" {
			sources = append(sources, q.Baseline)
		}
	}
	for _, uuid := range sources {
		rid, ok := content.AvailableByUUID(tx, uuid)
		if !ok {
			continue
		}
		data, err := g.cache.Expand(tx, rid)
		if err != nil {
			continue
		}
		parent, err := deck.Parse(data)
		if err != nil || parent.Type != deck.Checkin {
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
	cache := g.cache
	if !isTree(d) || d.B != "" || g.parentTreeGap(tx, d) != "" {
		return false, nil
	}
	c, err := content.StoreCone(tx)
	if err != nil || c == nil {
		return false, err
	}
	for _, uuid := range d.P {
		parent, ok := content.AvailableByUUID(tx, uuid)
		if !ok {
			return false, nil
		}
		data, err := cache.Expand(tx, parent)
		if err != nil {
			return false, nil
		}
		pd, err := deck.Parse(data)
		if err != nil {
			return false, nil
		}
		if _, err := checkinFileCards(tx, cache, pd); err != nil {
			return false, nil
		}
	}
	if g.coneReferences == nil {
		g.coneReferences, err = content.LoadConeReferences(ctx, tx, c)
		if err != nil {
			return false, err
		}
	}
	refs := g.coneReferences
	w := &treeWalk{loader: &subtreeLoader{q: tx, cache: cache}, stack: []treeFrame{{cards: d.S}}}
	w.skipMissing = func(uuid string) bool {
		var node libfossil.FslID
		if err := tx.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&node); err != nil {
			return false
		}
		return refs.Outside(node)
	}
	for len(w.stack) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := w.step(); err != nil {
			return false, nil
		}
	}
	files, err := checkinFileCardsFiltered(tx, cache, d, c)
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
	if err := crosslinkCheckinMetadata(tx, rid, d); err != nil {
		return false, err
	}
	if err := applyInlineTags(tx, rid, d, leaves); err != nil {
		return false, err
	}
	return true, nil
}
