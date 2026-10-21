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

// ConeReferenceFacts holds immutable raw-artifact facts for one database
// snapshot, without a cone key. The owner must validate that snapshot before
// using these facts against a database that may have changed.
//
// Preparation reads and inflates the whole store. Classify avoids that I/O,
// but still examines all parsed references and walks all check-in roots; it
// is not a bounded-per-round serving index.
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
	if ctx == nil || q == nil {
		panic("content.LoadConeReferenceFacts: nil context or querier")
	}
	facts := &ConeReferenceFacts{
		names: map[libfossil.FslID]string{}, held: map[string]libfossil.FslID{},
		nodes: map[libfossil.FslID]*deck.Deck{}, byUUID: map[string]*deck.Deck{},
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
		facts.names[rid] = uuid
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	err = ExpandAll(q, func(rid libfossil.FslID, uuid string, data []byte, expandErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if expandErr != nil {
			facts.unreadable = append(facts.unreadable, uuid)
			return nil
		}
		facts.held[uuid] = rid
		d, err := deck.Parse(data)
		if err != nil {
			return nil
		}
		if len(facts.byUUID)+len(facts.roots) >= coneReferenceNodes {
			return fmt.Errorf("sparse cone: artifact traversal bound exceeded")
		}
		facts.records = append(facts.records, coneReferenceRecord{uuid: uuid, deck: d})
		if d.Type == deck.Submanifest {
			facts.byUUID[uuid] = d
			facts.nodes[rid] = d
		}
		if d.Type == deck.Checkin {
			facts.roots = append(facts.roots, d)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return facts, nil
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
