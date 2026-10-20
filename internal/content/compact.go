package content

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// CompactStore turns held file content named only outside the store cone into
// recoverable phantoms. Dense stores (including lost policy) are unchanged.
// Structural artifacts, unknown content and retained delta dependencies stay
// held. Out-of-cone tree nodes stay held, as required by this store's crosslink
// invariant: a check-in is linked only when its whole tree can be enumerated.
// The existing write transaction excludes competing writers and rolls back
// every content change together on cancellation, corruption or SQL failure.
func CompactStore(ctx context.Context, d *db.DB) (int, error) {
	if ctx == nil {
		panic("content.CompactStore: context must not be nil")
	}
	if d == nil {
		panic("content.CompactStore: DB must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count := 0
	err := d.WithTx(func(tx *db.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := StoreCone(tx)
		if err != nil {
			return err
		}
		if c == nil {
			return nil
		}
		drop, err := compactCandidates(ctx, tx, c)
		if err != nil {
			return err
		}
		if err := compactRetainBases(tx, drop); err != nil {
			return err
		}
		var ordered []libfossil.FslID
		for rid := range drop {
			ordered = append(ordered, rid)
		}
		slices.Sort(ordered)
		for _, rid := range ordered {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE blob SET size=-1, content=NULL WHERE rid=?", rid); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT OR IGNORE INTO phantom(rid) VALUES(?)", rid); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM delta WHERE rid=?", rid); err != nil {
				return err
			}
			count++
		}
		return ctx.Err()
	})
	if err != nil {
		return 0, fmt.Errorf("content.CompactStore: %w", err)
	}
	if count < 0 {
		panic("content.CompactStore: negative compacted count")
	}
	return count, nil
}

type compactPlan struct {
	held                            map[string]libfossil.FslID
	outside, inside, keep, required map[string]bool
}

func compactCandidates(ctx context.Context, tx *db.Tx, c *cone.Cone) (map[libfossil.FslID]bool, error) {
	p := compactPlan{
		held: map[string]libfossil.FslID{}, outside: map[string]bool{}, inside: map[string]bool{},
		keep: map[string]bool{}, required: map[string]bool{},
	}
	err := ExpandAll(tx, func(rid libfossil.FslID, uuid string, data []byte, expandErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if expandErr != nil {
			return expandErr
		}
		p.held[uuid] = rid
		if len(data) == 0 {
			return nil
		}
		artifact, err := deck.Parse(data)
		if err != nil {
			return nil
		} // Ordinary file content is not an artifact deck.
		p.keep[uuid] = true
		p.references(c, artifact)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for uuid := range p.required {
		if _, ok := p.held[uuid]; !ok {
			return nil, fmt.Errorf("incomplete spine: artifact %s is not held", uuid)
		}
		p.keep[uuid] = true
	}
	drop := map[libfossil.FslID]bool{}
	for uuid, rid := range p.held {
		if p.outside[uuid] && !p.inside[uuid] && !p.keep[uuid] {
			drop[rid] = true
		}
	}
	return drop, nil
}

// Read actual cards rather than trust disposable crosslink tables. Historical
// names and shared aliases therefore protect content even before a rebuild.
func (p *compactPlan) references(c *cone.Cone, artifact *deck.Deck) {
	for _, f := range artifact.F {
		if c.Covers(f.Name) {
			p.inside[f.UUID] = true
		} else {
			p.outside[f.UUID] = true
		}
	}
	if artifact.B != "" {
		p.required[artifact.B] = true
	}
	for _, s := range artifact.S {
		p.required[s.UUID] = true
	}
	for _, parent := range artifact.P {
		p.required[parent] = true
	}
	for _, q := range artifact.Q {
		p.required[q.Target] = true
		if q.Baseline != "" {
			p.required[q.Baseline] = true
		}
	}
	for _, tag := range artifact.T {
		if tag.UUID != "*" && tag.UUID != "" {
			p.keep[tag.UUID] = true
		}
	}
	if artifact.A != nil {
		if artifact.A.Source != "" {
			p.keep[artifact.A.Source] = true
		}
	}
	// Cluster M cards are inventories, not dependencies: retaining their targets
	// would make every clustered file immune to explicit compaction.
}

// Unsent or private bytes may have no peer that can ever restore them.
func compactRetainLocal(tx *db.Tx, drop map[libfossil.FslID]bool) error {
	rows, err := tx.Query("SELECT rid FROM unsent UNION SELECT rid FROM private")
	if err != nil {
		return err
	}
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return errors.Join(err, rows.Close())
		}
		delete(drop, rid)
	}
	return errors.Join(rows.Err(), rows.Close())
}

// Preserve the transitive source closure of everything that will remain held.
// No retained delta can come to depend on a phantom as a result of compaction.
func compactRetainBases(tx *db.Tx, drop map[libfossil.FslID]bool) error {
	if err := compactRetainLocal(tx, drop); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT d.rid, d.srcid FROM delta d JOIN blob b ON b.rid=d.rid WHERE b.size>=0")
	if err != nil {
		return err
	}
	sources := map[libfossil.FslID]libfossil.FslID{}
	for rows.Next() {
		var rid, source libfossil.FslID
		if err := rows.Scan(&rid, &source); err != nil {
			return errors.Join(err, rows.Close())
		}
		sources[rid] = source
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for rid := range sources {
		if drop[rid] {
			continue
		}
		for source := sources[rid]; source > 0; source = sources[source] {
			if !drop[source] {
				break
			}
			delete(drop, source)
		}
	}
	return nil
}
