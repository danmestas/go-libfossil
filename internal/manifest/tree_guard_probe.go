package manifest

import (
	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
)

// ProbeTreeCycle exercises the real repeated-artifact guard at a walker-state
// boundary. uuid must name a stored leaf. Content addressing makes a cyclic
// artifact graph unconstructible, so only the active-frame identity is primed;
// node resolution and the next walk step are unchanged. This internal-package
// probe has no callers in production and adds no configurable walk behavior.
func ProbeTreeCycle(q db.Querier, uuid string) error {
	loader := &subtreeLoader{q: q}
	node, rid, err := loader.load(uuid)
	if err != nil {
		return err
	}
	if len(node.F) == 0 {
		panic("manifest.ProbeTreeCycle: uuid must name a leaf")
	}
	card := deck.SubmanifestCard{Name: node.F[0].Name, UUID: uuid}
	w := &treeWalk{
		loader: loader,
		stack: []treeFrame{
			{cards: []deck.SubmanifestCard{card}},
			{rid: rid, cards: []deck.SubmanifestCard{card}},
		},
	}
	return w.step()
}

// ProbeTreeVisitLimit resolves a stored node at the last allowed visit, then
// attempts the first excess visit through the actual loader. Priming the visit
// counter avoids manufacturing a million artifacts and changes neither the
// production limit nor the loader. Results are the last allowed and first
// excess resolution errors, respectively.
func ProbeTreeVisitLimit(q db.Querier, uuid string) (error, error) {
	loader := &subtreeLoader{q: q, resolved: maxTreeNodes - 1}
	_, _, lastAllowed := loader.load(uuid)
	_, _, firstExcess := loader.load(uuid)
	return lastAllowed, firstExcess
}
