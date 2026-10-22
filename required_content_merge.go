package libfossil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	internalsync "github.com/danmestas/go-libfossil/internal/sync"
)

// ErrBelowHorizon is matched by the error of an operation that needs history
// below the store's structural horizon (draft-fossil-horizon-00 §5.5), such
// as a merge whose base lies there. The message names the horizon and the
// remedy, a deepen (Repo.DeepenHorizon).
var ErrBelowHorizon = content.ErrBelowHorizon

// MergeContext is Merge with the remedy of draft-fossil-horizon-00 §5.5. When
// the merge base lies below the store's structural horizon, or the walk that
// finds it crosses the horizon, and Peer is given, it asks the origin for
// the base (§6.2.4), deepens the horizon to reach it (Repo.DeepenHorizon),
// and merges once more over the base the origin named, which the store then
// holds. Without a Peer, or when the merge does not cross the horizon, it is
// Merge. A merge never runs against a base the store does not hold.
func (c *Checkout) MergeContext(
	ctx context.Context, opts CheckoutMergeOpts, required RequiredContentOpts,
) (CheckoutMergeResult, error) {
	if err := required.validate(ctx); err != nil {
		return CheckoutMergeResult{}, err
	}
	res, crossing := c.Merge(opts)
	if !errors.Is(crossing, ErrBelowHorizon) || required.Peer == nil {
		return res, crossing
	}
	r := c.inner.Repo()
	_, current, err := c.Version()
	if err != nil {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: %w", err)
	}
	var side string
	if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", opts.Version).Scan(&side); err != nil {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: %w", err)
	}
	peer := &transportAdapter{pub: required.Peer}
	ask := required.internalSync()
	ask.MergeBaseOf = [2]string{current, side}
	answer, err := internalsync.Sync(ctx, r, peer, ask)
	if err != nil {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: asking the origin for the merge base: %w", err)
	}
	if !answer.MergeBaseAnswered {
		return CheckoutMergeResult{}, fmt.Errorf("%w; the origin did not answer a merge-base query", crossing)
	}
	if answer.MergeBase == "" {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: %s shares no history with %s, the origin says", side, current)
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
