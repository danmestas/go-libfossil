package libfossil

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/content"
	internalsync "github.com/danmestas/go-libfossil/internal/sync"
)

// ErrBelowHorizon is matched by the error of an operation that needs history
// below the store's structural horizon (draft-fossil-horizon-00 §5.5), such
// as a merge whose base lies there. The message names the horizon and the
// remedy, a deepen (Repo.DeepenHorizon).
var ErrBelowHorizon = content.ErrBelowHorizon

// MergeContext is Merge with the remedy of draft-fossil-horizon-00 §5.5. When
// the merge base lies below the store's structural horizon and Peer is
// given, it asks the origin for the base (§6.2.4), deepens the horizon to
// reach it (Repo.DeepenHorizon), and merges once more over the real base the
// store then holds. Without a Peer, or when the merge does not cross the
// horizon, it is Merge. A merge never runs against a base the store does not
// hold.
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
	if _, err := internalsync.Deepen(ctx, r, peer, answer.MergeBase, required.internalSync()); err != nil {
		return CheckoutMergeResult{}, fmt.Errorf("libfossil: merge: deepening to the base %s: %w", answer.MergeBase, err)
	}
	return c.Merge(opts)
}
