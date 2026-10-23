package libfossil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	internalsync "github.com/danmestas/go-libfossil/internal/sync"
)

// ErrBelowHorizon is matched by the error of an operation that needs history
// below the store's structural horizon (draft-fossil-horizon-00 §5.5), such
// as a merge whose base lies there. The message names the horizon and the
// remedy, a deepen (Repo.DeepenHorizon).
var ErrBelowHorizon = content.ErrBelowHorizon

// mergeBaseNodes bounds the walk that decides whether a merge's ancestry
// crosses the horizon, as the origin bounds its merge-base walk.
const mergeBaseNodes = 1 << 20

// maxMergeBaseSecondaries is the most secondaries a merge-base query carries
// (draft-fossil-horizon-00 §6.2.4): the checkout and one pending merge.
const maxMergeBaseSecondaries = 2

// MergeContext is Merge with the remedy of draft-fossil-horizon-00 §5.5.
// Under a structural horizon, when Peer is given and the ancestry of either
// side crosses the horizon, it asks the origin for the base (§6.2.4) even
// when the store's own walk finds one, since over part of the graph the
// common ancestor found need not be the nearest. It deepens the horizon to
// the base the origin names when the store does not hold it
// (Repo.DeepenHorizon) and merges over that base. When the origin does not
// answer it is Merge, which refuses a merge whose own walk crossed the
// horizon. Without a Peer, or when nothing crosses, it is Merge. A merge
// never runs against a base the store does not hold.
//
// The query names the merged-in version over the checkout's version and each
// plain merge pending in it. It carries at most two such secondaries, so a
// merge that must ask with two merges already pending is refused: asking
// without them would name a base Fossil's merge would not choose.
func (c *Checkout) MergeContext(
	ctx context.Context, opts CheckoutMergeOpts, required RequiredContentOpts,
) (CheckoutMergeResult, error) {
	if err := required.validate(ctx); err != nil {
		return CheckoutMergeResult{}, err
	}
	if required.Peer == nil {
		return c.Merge(opts)
	}
	r := c.inner.Repo()
	secondaries, err := c.inner.MergeSecondaries()
	if err != nil {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: %w", err)
	}
	starts := append([]fsltype.FslID{fsltype.FslID(opts.Version)}, secondaries...)
	crosses, err := content.AncestryCrosses(r.DB(), starts, mergeBaseNodes)
	if err != nil {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: %w", err)
	}
	if !crosses || opts.Version == int64(secondaries[0]) {
		return c.Merge(opts)
	}
	if len(secondaries) > maxMergeBaseSecondaries {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: %w: the merge base may lie below it, and a "+
			"merge-base query carries the checkout and at most one pending merge (horizon §6.2.4), "+
			"not the %d pending here; commit or undo the pending merges first",
			ErrBelowHorizon, len(secondaries)-1)
	}
	q := internalsync.MergeBaseQuery{}
	for i, rid := range starts {
		var uuid string
		if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: %w", err)
		}
		if i == 0 {
			q.Primary = uuid
		} else {
			q.Secondaries = append(q.Secondaries, uuid)
		}
	}
	peer := &transportAdapter{pub: required.Peer}
	ask := required.internalSync()
	ask.MergeBaseOf = q
	answer, err := internalsync.Sync(ctx, r, peer, ask)
	if err != nil {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: asking the origin for the merge base: %w", err)
	}
	if !answer.MergeBaseAnswered {
		res, err := c.Merge(opts)
		if errors.Is(err, ErrBelowHorizon) {
			return res, fmt.Errorf("%w; the origin did not answer a merge-base query", err)
		}
		return res, err
	}
	if answer.MergeBase == "" {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: %s shares no history with %s, the origin says",
			q.Primary, q.Secondaries[0])
	}
	base, held, err := heldCheckin(r.DB(), answer.MergeBase)
	if err != nil {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: the base %s: %w", answer.MergeBase, err)
	}
	if !held {
		if _, err := internalsync.Deepen(ctx, r, peer, answer.MergeBase, required.internalSync()); err != nil {
			return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: deepening to the base %s: %w", answer.MergeBase, err)
		}
		if base, _, err = heldCheckin(r.DB(), answer.MergeBase); err != nil {
			return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: the base %s: %w", answer.MergeBase, err)
		}
	}
	// The store still holds part of the graph, so its own walk may cross
	// the horizon again: the origin's base is the merge's, as --baseline.
	return c.merge(opts, base)
}

// heldCheckin is the rid of the artifact named uuid, 0 when the store has no
// such artifact, and whether it holds it as a check-in.
func heldCheckin(q db.Querier, uuid string) (int64, bool, error) {
	var rid int64
	var held bool
	err := q.QueryRow(`SELECT rid, size>=0 AND EXISTS(SELECT 1 FROM event WHERE objid=rid AND type='ci')
		FROM blob WHERE uuid=?`, uuid).Scan(&rid, &held)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return rid, held, err
}
