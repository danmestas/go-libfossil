package manifest

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// LinkRequiredArtifacts derives only the operation's verified retained closure.
// Unlike opportunistic dephantomization, a database failure is fatal to the
// caller's enclosing recovery transaction. Legitimate unavailable outside files
// retain the ordinary deferred-checkin representation for later synchronization.
func LinkRequiredArtifacts(
	ctx context.Context, r *repo.Repo, cache *content.Cache, rids []libfossil.FslID,
) (int, error) {
	if ctx == nil || r == nil || cache == nil {
		panic("manifest.LinkRequiredArtifacts: nil context, repo or verified cache")
	}
	if len(rids) > 1<<14 {
		return 0, fmt.Errorf("sparse required content: crosslink closure limit exceeded")
	}
	linked := 0
	err := r.WithTx(func(tx *db.Tx) error {
		if err := prepareCrosslinkTables(tx); err != nil {
			return err
		}
		guard, schedule := newCheckinDeferralGuard(), newBatchSchedule()
		guard.cache, guard.awaitFiles = cache, true
		for _, rid := range rids {
			if err := ctx.Err(); err != nil {
				return err
			}
			data, err := cache.Expand(tx, rid)
			if err != nil {
				return err
			}
			d, err := deck.Parse(data)
			if err != nil || !isArtifact(d) {
				continue
			}
			replayed, err := parsedArtifactReplayed(tx, rid, d)
			if err != nil {
				return err
			}
			if replayed {
				continue
			}
			if d.Type == deck.Checkin {
				deferred, err := guard.shouldDefer(tx, "RequiredContent", rid, d)
				if err != nil {
					return err
				}
				if deferred {
					continue
				}
			}
			handled, err := linkArtifact(tx, rid, d, cache, schedule)
			if err != nil {
				return fmt.Errorf("sparse required content: crosslink rid=%d: %w", rid, err)
			}
			if handled {
				linked++
			}
		}
		return repairOnTx(tx, schedule)
	})
	if err != nil {
		return 0, err
	}
	return linked, nil
}
