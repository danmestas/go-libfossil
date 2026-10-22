package content

import (
	"context"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ConeReferenceFacts holds immutable reference facts for one database
// snapshot, without a cone key. The owner must validate that snapshot before
// using these facts against a database that may have changed.
//
// Loading names every artifact in the store, but reads raw bytes only where
// the crosslink tables cannot answer: never a file version the crosslinker
// found to be no artifact, and a check-in only when its link is incomplete
// or its tree is not reproduced by mlink (see answerable). It serves whole-store
// work (a pull's inventory, compaction, verification, crosslink), never a
// clone round. Retained decks are classification projections, not complete
// artifacts suitable for marshaling.
type ConeReferenceFacts struct {
	names      map[libfossil.FslID]string
	held       map[string]libfossil.FslID // Read and hash-verified.
	nodes      map[libfossil.FslID]*deck.Deck
	byUUID     map[string]*deck.Deck
	records    []coneReferenceRecord
	roots      []*deck.Deck
	unreadable []string
	// spine and files are linked check-ins' facts, from the tables: the
	// check-ins and their references, and their trees' (name, file) pairs.
	spine []string
	files []deck.FileCard
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
	facts, _, err := loadConeReferenceFactsBounded(ctx, q, defaultReferenceLimits.refs, true)
	return facts, err
}

// referenceLimits bounds one classification: its raw reference reads, and
// under a content horizon the tree walk of the held set, which is charged
// per check-in in that set rather than per artifact in the store.
type referenceLimits struct {
	refs           ReferenceBudget
	treePerCheckin int64
	treeMax        int64
}

// ExpandedBytes also pays for the delta chains a raw read must expand: a
// check-in whose parent never arrived is read raw, and in the Fossil SCM
// repository one such manifest sits 1,594 deltas from its full base, about
// 120 MiB of expansion. Bases are released as the chain advances, so this
// bounds work rather than memory. The tree budget scales with the held set
// so that a horizon's cost follows the history it keeps.
var defaultReferenceLimits = referenceLimits{
	refs: ReferenceBudget{
		Artifacts: coneReferenceNodes, StoredBytes: 64 << 20,
		InflatedBytes: 128 << 20, ExpandedBytes: 512 << 20, Cards: 1 << 20,
	},
	treePerCheckin: 1 << 20, treeMax: 4 << 30,
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
	for _, uuid := range facts.spine {
		p.required[uuid] = true
	}
	for _, f := range facts.files {
		if c.Covers(f.Name) {
			p.inside[f.UUID] = true
		} else {
			p.outside[f.UUID] = true
		}
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
