package sync

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/deck"
)

// coneTreeLevel is one node's S cards and the bound its slice lies below,
// empty meaning unbounded.
type coneTreeLevel struct {
	cards []deck.SubmanifestCard
	upper string
}

func (l coneTreeLevel) limit(i int) string {
	if i+1 < len(l.cards) {
		return l.cards[i+1].Name
	}
	return l.upper
}

type coneTreeRange struct {
	name, uuid, limit string
}

// tree descends the cards whose key range may hold a covered path and which
// the parent's tree does not carry over the same range, the §6.2 walk.
// priorFiles are the parent leaf's files when the child split that leaf.
func (w *coneWalk) tree(
	level, prior coneTreeLevel, priorFiles map[deck.FileCard]bool, depth int, out map[int]bool,
) error {
	same := make(map[coneTreeRange]bool, len(prior.cards))
	byName := make(map[string]coneTreeLevel, len(prior.cards))
	for i, card := range prior.cards {
		same[coneTreeRange{card.Name, card.UUID, prior.limit(i)}] = true
		byName[card.Name] = coneTreeLevel{cards: []deck.SubmanifestCard{card}, upper: prior.limit(i)}
	}
	for i, card := range level.cards {
		if err := w.visit(depth); err != nil {
			return err
		}
		limit := level.limit(i)
		if !w.cone.MayCover(card.Name, limit) || same[coneTreeRange{card.Name, card.UUID, limit}] {
			continue
		}
		next := coneTreeLevel{cards: []deck.SubmanifestCard{card}, upper: limit}
		if err := w.node(next, byName[card.Name], priorFiles, depth, out); err != nil {
			return err
		}
	}
	return nil
}

func (w *coneWalk) visit(depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if depth > coneWalkDepth || w.visits >= coneWalkVisits {
		return fmt.Errorf("sparse cone: tree traversal bound exceeded")
	}
	w.visits++
	return nil
}

// node sends one changed in-cone node and descends it. Each level holds the
// single card it stands for; prior is the parent's card of the same name.
func (w *coneWalk) node(
	at, prior coneTreeLevel, priorFiles map[deck.FileCard]bool, depth int, out map[int]bool,
) error {
	card := at.cards[0]
	d, rid, err := w.nodeDeck(card.UUID)
	if err != nil || d == nil {
		return err
	}
	if err := checkNodeRange(d, card.Name, at.upper); err != nil {
		return err
	}
	if w.path[card.UUID] {
		return fmt.Errorf("sparse cone: cyclic sub-manifest %s", card.UUID)
	}
	if !w.horizon {
		out[rid] = true // under a horizon a node goes out at its own row
	}
	nextFiles := priorFiles
	nextPrior := coneTreeLevel{}
	if len(prior.cards) == 1 {
		if p, _, err := w.nodeDeck(prior.cards[0].UUID); err != nil {
			return err
		} else if p != nil && len(p.F) > 0 {
			nextFiles = fileSet(p.F)
		} else if p != nil {
			nextPrior = coneTreeLevel{cards: p.S, upper: prior.upper}
		}
	}
	if len(d.F) > 0 {
		return w.files(d.F, nextFiles, out)
	}
	w.path[card.UUID] = true
	err = w.tree(coneTreeLevel{cards: d.S, upper: at.upper}, nextPrior, nextFiles, depth+1, out)
	delete(w.path, card.UUID)
	return err
}

// nodeDeck is the sub-manifest uuid names when this clone sends it. A node
// the store lacks is left for the recipient to classify from its spine.
func (w *coneWalk) nodeDeck(uuid string) (*deck.Deck, int, error) {
	rid, ok, err := w.servable(uuid)
	if err != nil || !ok {
		return nil, 0, err
	}
	if kind, d := w.classify(rid); kind == sparseNode {
		return d, rid, nil
	}
	return nil, 0, nil
}

func checkNodeRange(d *deck.Deck, name, limit string) error {
	first, last := "", ""
	switch {
	case len(d.F) > 0:
		first, last = d.F[0].Name, d.F[len(d.F)-1].Name
	case len(d.S) > 0:
		first, last = d.S[0].Name, d.S[len(d.S)-1].Name
	}
	if first == "" || first != name || limit != "" && last >= limit {
		return fmt.Errorf("sparse cone: invalid sub-manifest range")
	}
	return nil
}

func fileSet(files []deck.FileCard) map[deck.FileCard]bool {
	set := make(map[deck.FileCard]bool, len(files))
	for _, f := range files {
		set[deck.FileCard{Name: f.Name, UUID: f.UUID}] = true
	}
	return set
}
