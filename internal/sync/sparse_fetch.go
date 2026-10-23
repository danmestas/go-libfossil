package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// FetchCone applies lasting store policy and fetches newly wanted elision with
// ordinary pull. The caller supplies its configured peer and credentials; this
// helper neither discovers endpoints nor starts a background transfer. A failed
// transfer leaves the widened intent in place so an ordinary retry can heal it.
//
// Moving a structural horizon discards the held seed before anything else
// (draft-fossil-horizon-00 §5.8): the seed for the old C, applied at a later
// date than the new seed's propagation, would shadow it. A transfer that then
// fails puts the old cone and seed back, as zit's deepen does, since a store
// left structural without a seed derives no branch state for K at all.
func FetchCone(ctx context.Context, r *repo.Repo, peer Transport, next *cone.Cone, opts SyncOpts) (*SyncResult, error) {
	if ctx == nil || r == nil {
		panic("sync.FetchCone: nil context or repo")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, err := content.LoadAbsencesContext(ctx, r.DB())
	if err != nil {
		return nil, err
	}
	refs, err := content.LoadConeReferences(ctx, r.DB(), next)
	if err != nil {
		return nil, err
	}
	rows, err := r.DB().Query("SELECT rid FROM phantom")
	if err != nil {
		return nil, err
	}
	wanted := 0
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if a.Class(rid) == content.Elided && !refs.Outside(rid) {
			wanted++
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if wanted > 0 && peer == nil {
		return nil, fmt.Errorf("sparse cone: %d elided artifact(s); no configured peer to fetch %d newly covered artifact(s)", a.Count(content.Elided), wanted)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	old, err := content.StoreCone(r.DB())
	if err != nil {
		return nil, err
	}
	oldSeed, err := discardMovedSeed(r, old, next)
	if err != nil {
		return nil, err
	}
	if err := content.SetStoreCone(r.DB(), next); err != nil {
		if oldSeed != nil {
			err = errors.Join(err, restoreSeed(r, old, oldSeed))
		}
		return nil, err
	}
	if wanted == 0 && peer == nil {
		a, err := content.LoadAbsencesContext(ctx, r.DB())
		if err != nil {
			return nil, err
		}
		return &SyncResult{Elided: a.Count(content.Elided)}, nil
	}
	opts.Pull = true
	opts.Push = false
	res, err := Sync(ctx, r, peer, opts)
	if err != nil && oldSeed != nil {
		return res, errors.Join(err, restoreSeed(r, old, oldSeed))
	}
	return res, err
}

// discardMovedSeed discards the held seed when next moves the structural
// horizon of old, the store cone it replaces, and returns the seed's bytes;
// nil when nothing was discarded.
func discardMovedSeed(r *repo.Repo, old, next *cone.Cone) ([]byte, error) {
	h, ok := horizonOf(old)
	if !ok || h.Depth() != cone.DepthStructural {
		return nil, nil
	}
	if nh, ok := horizonOf(next); ok && nh == h {
		return nil, nil
	}
	held, err := seed.Held(r.DB())
	if err != nil || held == nil {
		return nil, err
	}
	if err := r.WithTx(func(tx *db.Tx) error { return seed.Discard(tx) }); err != nil {
		return nil, err
	}
	return held, nil
}

// restoreSeed puts back the store cone and seed a failed move replaced.
// The seed was held, so verified, before the move; it goes back without a
// verifier, and the next Apply re-checks it against the clone's key
// (draft-fossil-horizon-00 §5.2.4).
func restoreSeed(r *repo.Repo, old *cone.Cone, held []byte) error {
	if held == nil {
		panic("sync.restoreSeed: no seed to restore")
	}
	return r.WithTx(func(tx *db.Tx) error {
		if err := content.SetStoreCone(tx, old); err != nil {
			return err
		}
		return seed.Hold(tx, hash.SHA3(held), held, nil)
	})
}
