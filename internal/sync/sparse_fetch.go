package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// FetchCone applies lasting store policy and fetches newly wanted elision with
// ordinary pull. The caller supplies its configured peer and credentials; this
// helper neither discovers endpoints nor starts a background transfer. A failed
// transfer leaves the widened intent in place so an ordinary retry can heal it.
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
	if err := content.SetStoreCone(r.DB(), next); err != nil {
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
	return Sync(ctx, r, peer, opts)
}
