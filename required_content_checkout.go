package libfossil

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// CheckinContext recovers necessary parent structure from an explicit peer,
// outside checkout locks and repository transactions. It then gathers checkout
// changes afresh; no stale file list is replayed after network I/O.
func (c *Checkout) CheckinContext(
	ctx context.Context, opts CheckoutCommitOpts, required RequiredContentOpts,
) (int64, string, error) {
	if err := required.validate(ctx); err != nil {
		return 0, "", err
	}
	parent, state, err := c.requiredState()
	if err != nil {
		return 0, "", err
	}
	r := c.inner.Repo()
	err = manifest.RequireParentContent(ctx, r.DB(), []fsltype.FslID{parent})
	if err != nil {
		if err := required.recover(ctx, r, err); err != nil {
			return 0, "", err
		}
		if err := c.checkRequiredState(state); err != nil {
			return 0, "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	return c.Checkin(opts)
}

// SetConeContext recovers only structure needed to enumerate next and payloads
// newly entering the checkout. Explicit widening durably extends store coverage;
// an attempted fetch retains that intent even on failure. No-peer refusal does
// not change policy. Planning and local checks repeat before files move.
func (c *Checkout) SetConeContext(
	ctx context.Context, next *Cone, required RequiredContentOpts,
) (ConeChange, error) {
	if err := required.validate(ctx); err != nil {
		return ConeChange{}, err
	}
	parent, state, err := c.requiredState()
	if err != nil {
		return ConeChange{}, err
	}
	if required.Peer != nil || parent == 0 {
		state, err = c.requiredStorePolicy(next, state)
		if err != nil {
			return ConeChange{}, err
		}
	}
	if parent == 0 {
		return c.SetCone(next)
	}
	r := c.inner.Repo()
	var filter manifest.FileFilter
	if next != nil {
		filter = next.inner
	}
	files, err := manifest.ListFilesFiltered(r, parent, filter)
	if err != nil {
		refusal := manifest.RequireParentContent(ctx, r.DB(), []fsltype.FslID{parent})
		if refusal == nil {
			return ConeChange{}, err
		}
		if err := required.recover(ctx, r, refusal); err != nil {
			return ConeChange{}, err
		}
		if err := c.checkRequiredState(state); err != nil {
			return ConeChange{}, err
		}
		files, err = manifest.ListFilesFiltered(r, parent, filter)
		if err != nil {
			return ConeChange{}, err
		}
	}
	old := c.Cone()
	names, err := requiredFileNames(ctx, r, files, func(name string) bool {
		return old != nil && !old.Covers(name)
	})
	if err != nil {
		return ConeChange{}, err
	}
	if err := required.recoverFiles(ctx, r, names); err != nil {
		return ConeChange{}, err
	}
	if _, err := c.requiredStorePolicy(next, state); err != nil {
		return ConeChange{}, err
	}
	return c.SetCone(next)
}

func (c *Checkout) requiredState() (fsltype.FslID, string, error) {
	parent, uuid, err := c.inner.Version()
	if err != nil {
		return 0, "", err
	}
	state, err := requiredState(c.inner.Repo(), []fsltype.FslID{parent})
	if err != nil {
		return 0, "", err
	}
	if cone := c.Cone(); cone != nil {
		state += "\ncheckout-cone=" + cone.Digest()
	}
	return parent, fmt.Sprintf("%d:%s\n%s", parent, uuid, state), nil
}

func (c *Checkout) checkRequiredState(before string) error {
	_, after, err := c.requiredState()
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("libfossil: checkout snapshot or policy changed " +
			"during required-content recovery")
	}
	return nil
}
