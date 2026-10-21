package content

import (
	"context"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ConeReferenceFacts holds immutable raw-artifact facts for one database
// snapshot, without a cone key. The owner must validate that snapshot before
// using these facts against a database that may have changed.
//
// Loading reads and inflates the whole store, so it serves whole-store work
// (a pull's inventory, compaction, verification, crosslink), never a clone
// round. Retained decks are classification projections, not complete
// artifacts suitable for marshaling.
type ConeReferenceFacts struct {
	names      map[libfossil.FslID]string
	held       map[string]libfossil.FslID
	nodes      map[libfossil.FslID]*deck.Deck
	byUUID     map[string]*deck.Deck
	records    []coneReferenceRecord
	roots      []*deck.Deck
	unreadable []string
}

type coneReferenceRecord struct {
	uuid string
	deck *deck.Deck
}

func LoadConeReferenceFacts(ctx context.Context, q db.Querier) (*ConeReferenceFacts, error) {
	// Discovery is guarded before allocation. Counting only check-ins/trees
	// does not bound wiki, forum, cluster or file storage. Oversized
	// snapshots fail rather than allocate first and discover their budget
	// after parsing.
	facts, _, err := loadConeReferenceFactsBounded(ctx, q, ReferenceBudget{
		Artifacts: coneReferenceNodes, StoredBytes: 64 << 20,
		InflatedBytes: 128 << 20, ExpandedBytes: 128 << 20, Cards: 1 << 20,
	}, true)
	return facts, err
}

// Classify creates request-local membership; neither the facts nor previous
// classifications are changed. Maps of raw identities and decks are read-only.
func (facts *ConeReferenceFacts) Classify(
	ctx context.Context, c *cone.Cone,
) (*ConeReferences, error) {
	if facts == nil || ctx == nil {
		panic("content.ConeReferenceFacts.Classify: nil facts or context")
	}
	p := &ConeReferences{
		names: facts.names, held: facts.held, nodes: facts.nodes,
		outside: map[string]bool{}, inside: map[string]bool{},
		required: map[string]bool{}, tree: map[string]bool{},
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil {
		return p, nil
	}
	for _, uuid := range facts.unreadable {
		p.required[uuid] = true
	}
	for _, record := range facts.records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d := record.deck
		if d.Type != deck.Submanifest {
			p.required[record.uuid] = true
		}
		for _, f := range d.F {
			if f.UUID == "" {
				continue
			}
			if c.Covers(f.Name) {
				p.inside[f.UUID] = true
			} else {
				p.outside[f.UUID] = true
			}
		}
		p.structural(d)
	}
	w := coneReferenceWalk{ctx: ctx, cone: c, refs: p, nodes: facts.byUUID, path: map[string]bool{}}
	for _, root := range facts.roots {
		if err := w.walk(root.S, "", 1, true); err != nil {
			return nil, err
		}
	}
	return p, nil
}
