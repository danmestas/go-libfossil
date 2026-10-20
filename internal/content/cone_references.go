package content

import (
	"context"
	"errors"
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
	rows, err := q.Query("SELECT rid,uuid FROM blob")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var rid libfossil.FslID
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		p.names[rid] = uuid
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	nodes := map[string]*deck.Deck{}
	var roots []*deck.Deck
	err = ExpandAll(q, func(rid libfossil.FslID, uuid string, data []byte, expandErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if expandErr != nil {
			p.required[uuid] = true
			return nil
		} // Unreadable bytes may be structural; no outside proof permits deletion.
		p.held[uuid] = rid
		d, err := deck.Parse(data)
		if err != nil {
			return nil
		} // Ordinary file bytes.
		if len(nodes)+len(roots) >= coneReferenceNodes {
			return fmt.Errorf("sparse cone: artifact traversal bound exceeded")
		}
		if d.Type == deck.Submanifest {
			nodes[uuid] = d
			p.nodes[rid] = d
		} else {
			p.required[uuid] = true
		}
		if d.Type == deck.Checkin {
			roots = append(roots, d)
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
		return nil
	})
	if err != nil {
		return nil, err
	}
	w := coneReferenceWalk{ctx: ctx, cone: c, refs: p, nodes: nodes, path: map[string]bool{}}
	for _, root := range roots {
		if err := w.walk(root.S, "", 1, true); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// RetainProvenance removes raw proof dependencies from a proposed drop set.
// Leaves and nonterminal paths stay held: only an interior whose direct children
// are held leaves can disappear without severing root-to-phantom range proofs.
// This conservative rule does not depend on disposable crosslink indexes.
func (p *ConeReferences) RetainProvenance(ctx context.Context, drop map[libfossil.FslID]bool) error {
	keep := map[libfossil.FslID]bool{}
	for rid, node := range p.nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(node.F) > 0 {
			keep[rid] = true
		}
		if drop[rid] {
			for _, child := range node.S {
				childRID := p.held[child.UUID]
				keep[childRID] = true
				childNode := p.nodes[childRID]
				if childNode == nil || len(childNode.F) == 0 {
					keep[rid] = true
				}
			}
		}
	}
	for rid := range keep {
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
