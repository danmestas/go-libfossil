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

// CompactStore turns proved outside content into recoverable phantoms. Dense
// stores (including lost policy) are unchanged. Structural artifacts, unknown
// content, classification provenance and retained delta dependencies stay held.
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

func compactCandidates(ctx context.Context, tx *db.Tx, c *cone.Cone) (map[libfossil.FslID]bool, error) {
	refs, err := LoadConeReferences(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	held := map[string]libfossil.FslID{}
	nodes := map[libfossil.FslID]*deck.Deck{}
	drop := map[libfossil.FslID]bool{}
	// Classification tolerates unavailable bytes. Destructive compaction must
	// separately verify every held payload before changing any holdings.
	err = ExpandAll(tx, func(rid libfossil.FslID, uuid string, data []byte, expandErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if expandErr != nil {
			return expandErr
		}
		held[uuid] = rid
		if refs.Outside(rid) {
			drop[rid] = true
		}
		if len(data) == 0 {
			return nil
		}
		artifact, err := deck.Parse(data)
		if err == nil && artifact.Type == deck.Submanifest {
			nodes[rid] = artifact
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for uuid := range refs.required {
		if _, ok := held[uuid]; !ok {
			return nil, fmt.Errorf("incomplete spine: artifact %s is not held", uuid)
		}
	}
	for rid, uuid := range refs.names {
		if refs.Required(rid) {
			if _, ok := held[uuid]; !ok {
				return nil, fmt.Errorf("incomplete spine: artifact %s is not held", uuid)
			}
		}
	}
	compactRetainProvenance(nodes, held, drop)
	return drop, nil
}

// Without raw F/S provenance, a phantom would become unknown on the next
// classification. Retain leaves and all nonterminal paths: only an interior
// whose direct children are held leaves can disappear without severing the
// root-to-phantom range proof. Derived indexes need never have existed.
func compactRetainProvenance(nodes map[libfossil.FslID]*deck.Deck,
	held map[string]libfossil.FslID, drop map[libfossil.FslID]bool,
) {
	keep := map[libfossil.FslID]bool{}
	for rid, node := range nodes {
		if len(node.F) > 0 {
			keep[rid] = true
		}
		if drop[rid] {
			for _, child := range node.S {
				childRID := held[child.UUID]
				keep[childRID] = true
				childNode := nodes[childRID]
				if childNode == nil {
					keep[rid] = true
				} else if len(childNode.F) == 0 {
					keep[rid] = true
				}
			}
		}
	}
	for rid := range keep {
		delete(drop, rid)
	}
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
