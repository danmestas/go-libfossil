package libfossil

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// UpdateContext recovers necessary structure and selected update payloads from
// an explicit peer before checkout mutation. An implicit target is resolved once;
// after recovery the local update replans against fresh working files.
func (c *Checkout) UpdateContext(
	ctx context.Context, opts UpdateOpts, required RequiredContentOpts,
) (UpdateResult, error) {
	if err := required.validate(ctx); err != nil {
		return UpdateResult{}, err
	}
	_, before, err := c.requiredState()
	if err != nil {
		return UpdateResult{}, err
	}
	target, files, readiness := c.inner.UpdateRequiredFiles(ctx, checkout.UpdateOpts{
		TargetRID: fsltype.FslID(opts.TargetRID),
	})
	if err := c.checkRequiredState(before); err != nil {
		return UpdateResult{}, err
	}
	if target == 0 {
		return UpdateResult{}, readiness
	}
	state, err := c.updateRequiredState(target)
	if err != nil {
		return UpdateResult{}, err
	}
	if readiness != nil {
		if err := required.recover(ctx, c.inner.Repo(), readiness); err != nil {
			return UpdateResult{}, err
		}
		if err := c.checkUpdateRequiredState(target, state); err != nil {
			return UpdateResult{}, err
		}
		resolved, fresh, err := c.inner.UpdateRequiredFiles(ctx, checkout.UpdateOpts{TargetRID: target})
		if err != nil {
			return UpdateResult{}, err
		}
		if resolved != target {
			return UpdateResult{}, fmt.Errorf("libfossil: update recovery target changed")
		}
		files = fresh
	}
	names, err := requiredFileNames(ctx, c.inner.Repo(), files, func(string) bool { return true })
	if err != nil {
		return UpdateResult{}, err
	}
	if err := required.recoverFiles(ctx, c.inner.Repo(), names); err != nil {
		return UpdateResult{}, err
	}
	if err := c.checkUpdateRequiredState(target, state); err != nil {
		return UpdateResult{}, err
	}
	return c.update(ctx, UpdateOpts{TargetRID: int64(target)})
}

func (c *Checkout) updateRequiredState(target fsltype.FslID) (string, error) {
	_, state, err := c.requiredState()
	if err != nil {
		return "", err
	}
	identity, err := requiredState(c.inner.Repo(), []fsltype.FslID{target})
	if err != nil {
		return "", err
	}
	return state + "\nupdate-target=" + identity, nil
}

func (c *Checkout) checkUpdateRequiredState(target fsltype.FslID, before string) error {
	after, err := c.updateRequiredState(target)
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("libfossil: update snapshot or policy changed during recovery")
	}
	return nil
}
