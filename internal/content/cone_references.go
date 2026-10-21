package content

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

const coneReferenceDepth = 16
const coneReferenceNodes = 1 << 20

// ConeReferences is inferred from raw artifacts, not crosslink tables: a sparse
// tree can be healthy before its elided children permit whole-tree crosslinking.
// Unknown artifacts stay wanted. A covered alias or structural reference wins
// over every outside reference.
type ConeReferences struct {
	names                     map[libfossil.FslID]string
	outside, inside, required map[string]bool
	tree                      map[string]bool
	held                      map[string]libfossil.FslID
	nodes                     map[libfossil.FslID]*deck.Deck
}

// Outside reports only proven elision candidates. Delta dependency closure is
// a separate operation: a sender can instead expand a retained delta in full.
func (p *ConeReferences) Outside(rid libfossil.FslID) bool {
	if p == nil {
		return false
	}
	uuid := p.names[rid]
	return p.outside[uuid] && !p.inside[uuid] && !p.required[uuid]
}

// Required reports a covered alias or a structural reference, including ones
// which have not been crosslinked yet.
func (p *ConeReferences) Required(rid libfossil.FslID) bool {
	if p == nil {
		return false
	}
	uuid := p.names[rid]
	return p.inside[uuid] || p.required[uuid]
}

// Artifacts counts the artifact names the classification read.
func (p *ConeReferences) Artifacts() int {
	if p == nil {
		return 0
	}
	return len(p.names)
}

// LoadConeReferences classifies both held and phantom artifact names. Traversal
// shares the single-name lookup depth and node limits across all roots.
func LoadConeReferences(ctx context.Context, q db.Querier, c *cone.Cone) (*ConeReferences, error) {
	if ctx == nil || q == nil {
		panic("content.LoadConeReferences: nil context or querier")
	}
	p := &ConeReferences{names: map[libfossil.FslID]string{}, outside: map[string]bool{}, inside: map[string]bool{}, required: map[string]bool{}, tree: map[string]bool{}, held: map[string]libfossil.FslID{}, nodes: map[libfossil.FslID]*deck.Deck{}}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil {
		return p, nil
	}
	facts, err := LoadConeReferenceFacts(ctx, q)
	if err != nil {
		return nil, err
	}
	return facts.Classify(ctx, c)
}

// RetainProvenance removes every held sub-manifest node from a proposed drop
// set. Sparse-cone Sections 5.4 and 6.3 keep manifests and sub-manifest nodes,
// in cone or not: a leaf's F cards are what classify the blobs it names as
// elided rather than transit phantoms, and an interior is the range proof that
// reaches the leaf. Only blobs are ever dropped. Check-ins never enter a drop
// set, since no S card names them.
func (p *ConeReferences) RetainProvenance(ctx context.Context, drop map[libfossil.FslID]bool) error {
	for rid := range p.nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		delete(drop, rid)
	}
	return ctx.Err()
}

func (p *ConeReferences) structural(d *deck.Deck) {
	for _, uuid := range []string{d.B, d.G, d.I} {
		if uuid != "" {
			p.required[uuid] = true
		}
	}
	for _, uuid := range d.P {
		p.required[uuid] = true
	}
	for _, q := range d.Q {
		p.required[q.Target] = true
		if q.Baseline != "" {
			p.required[q.Baseline] = true
		}
	}
	for _, t := range d.T {
		if t.UUID != "" && t.UUID != "*" {
			p.required[t.UUID] = true
		}
	}
	if d.A != nil && d.A.Source != "" {
		p.required[d.A.Source] = true
	}
	// Cluster inventories are not dependencies; otherwise every clustered file
	// would be immune to sparse transfer and explicit compaction.
}

type coneReferenceWalk struct {
	ctx     context.Context
	cone    *cone.Cone
	refs    *ConeReferences
	nodes   map[string]*deck.Deck
	path    map[string]bool
	visited int
}

func (w *coneReferenceWalk) walk(cards []deck.SubmanifestCard, upper string, depth int, parentWanted bool) error {
	for i, card := range cards {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if depth > coneReferenceDepth || w.visited >= coneReferenceNodes {
			return fmt.Errorf("sparse cone: tree traversal bound exceeded")
		}
		w.visited++
		w.refs.tree[card.UUID] = true
		limit := upper
		if i+1 < len(cards) {
			limit = cards[i+1].Name
		}
		wanted := parentWanted && w.cone.MayCover(card.Name, limit)
		if wanted {
			w.refs.required[card.UUID] = true
		} else {
			w.refs.outside[card.UUID] = true
		}
		d := w.nodes[card.UUID]
		if d == nil {
			continue
		}
		if w.path[card.UUID] {
			return fmt.Errorf("sparse cone: cyclic sub-manifest %s", card.UUID)
		}
		if len(d.F) > 0 {
			if d.F[0].Name != card.Name || limit != "" && d.F[len(d.F)-1].Name >= limit {
				return fmt.Errorf("sparse cone: invalid sub-manifest range")
			}
			continue
		}
		if len(d.S) == 0 || d.S[0].Name != card.Name || limit != "" && d.S[len(d.S)-1].Name >= limit {
			return fmt.Errorf("sparse cone: invalid sub-manifest range")
		}
		w.path[card.UUID] = true
		err := w.walk(d.S, limit, depth+1, wanted)
		delete(w.path, card.UUID)
		if err != nil {
			return err
		}
	}
	return nil
}
