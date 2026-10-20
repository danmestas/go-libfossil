package content

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
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
	if ctx == nil || d == nil {
		panic("content.CompactStore: context and DB must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count := 0
	err := d.WithTx(func(tx *db.Tx) error {
		c, err := StoreCone(tx)
		if err != nil || c == nil {
			return err
		}
		held := map[string]libfossil.FslID{}
		out, in, keep := map[string]bool{}, map[string]bool{}, map[string]bool{}
		required := map[string]bool{}
		err = ExpandAll(tx, func(rid libfossil.FslID, uuid string, data []byte, expandErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if expandErr != nil {
				return expandErr
			}
			held[uuid] = rid
			artifact, err := deck.Parse(data)
			if err != nil {
				return nil
			} // Ordinary file content is not an artifact deck.
			keep[uuid] = true
			for _, f := range artifact.F {
				if c.Covers(f.Name) {
					in[f.UUID] = true
				} else {
					out[f.UUID] = true
				}
			}
			if artifact.B != "" {
				required[artifact.B] = true
			}
			for _, s := range artifact.S {
				required[s.UUID] = true
			}
			for _, p := range artifact.P {
				required[p] = true
			}
			for _, q := range artifact.Q {
				required[q.Target] = true
				if q.Baseline != "" {
					required[q.Baseline] = true
				}
			}
			for _, tag := range artifact.T {
				if tag.UUID != "*" && tag.UUID != "" {
					keep[tag.UUID] = true
				}
			}
			if artifact.A != nil && artifact.A.Source != "" {
				keep[artifact.A.Source] = true
			}
			return nil
		})
		if err != nil {
			return err
		}
		for uuid := range required {
			if _, ok := held[uuid]; !ok {
				return fmt.Errorf("incomplete spine: artifact %s is not held", uuid)
			}
			keep[uuid] = true
		}
		drop := map[libfossil.FslID]bool{}
		for uuid, rid := range held {
			if out[uuid] && !in[uuid] && !keep[uuid] {
				drop[rid] = true
			}
		}
		if err := compactRetainBases(tx, drop); err != nil {
			return err
		}
		for _, rid := range held {
			if !drop[rid] {
				continue
			}
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
	return count, nil
}

// Preserve the transitive source closure of everything that will remain held.
// No retained delta can come to depend on a phantom as a result of compaction.
func compactRetainBases(tx *db.Tx, drop map[libfossil.FslID]bool) error {
	rows, err := tx.Query("SELECT d.rid, d.srcid FROM delta d JOIN blob b ON b.rid=d.rid WHERE b.size>=0")
	if err != nil {
		return err
	}
	sources := map[libfossil.FslID]libfossil.FslID{}
	for rows.Next() {
		var rid, source libfossil.FslID
		if err := rows.Scan(&rid, &source); err != nil {
			rows.Close()
			return err
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
