package manifest

import (
	"context"
	"errors"
	"slices"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// ReadFileAt reads filePath as check-in rid names it, serving the manifest and
// the payload through cache (nil reads uncached); found is false when rid
// does not name the file. When the read needs an artifact the store does not
// hold and rid lies below the store cone's horizon, outside its held set, it
// fails with a *content.MissingBelowHorizon naming the check-in, when its
// structure is what is missing, or else the payload
// (draft-fossil-horizon-00 §4.4). Any other absence keeps its own error.
// place carries the held set across the reads of one operation; nil places
// it afresh.
func ReadFileAt(
	ctx context.Context, r *repo.Repo, rid libfossil.FslID, filePath string, cache *content.Cache,
	place *Placement,
) (data []byte, found bool, err error) {
	if ctx == nil || r == nil {
		panic("manifest.ReadFileAt: nil context or repo")
	}
	if filePath == "" {
		panic("manifest.ReadFileAt: filePath must not be empty")
	}
	f, found, err := LookupFileCached(r, rid, filePath, cache)
	if err != nil {
		missing, merr := MissingStructure(ctx, r.DB(), []libfossil.FslID{rid}, place)
		return nil, false, missingOr(missing, merr, err)
	}
	if !found {
		return nil, false, nil
	}
	fileRID, err := content.RequireAvailableByUUID(r.DB(), f.UUID)
	if err != nil {
		below, berr := place.Below(ctx, r.DB(), rid)
		if berr != nil || !below {
			return nil, false, err
		}
		missing, merr := content.MissingAtHorizon(r.DB(), nil, []string{f.UUID})
		return nil, false, missingOr(missing, merr, err)
	}
	data, err = cache.Expand(r.DB(), fileRID)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// MissingStructure is the refusal naming each check-in among rids, below the
// store cone's horizon, whose manifest, baseline or nodes the store does not
// hold; nil when there is none. place is as ReadFileAt takes it.
func MissingStructure(
	ctx context.Context, q db.Querier, rids []libfossil.FslID, place *Placement,
) (*content.MissingBelowHorizon, error) {
	if ctx == nil || q == nil {
		panic("manifest.MissingStructure: nil context or querier")
	}
	var checkins []string
	for _, rid := range rids {
		below, err := place.Below(ctx, q, rid)
		if err != nil {
			return nil, err
		}
		if !below {
			continue
		}
		err = RequireParentContent(ctx, q, []libfossil.FslID{rid})
		var required *RequiredContentError
		if errors.As(err, &required) {
			checkins = append(checkins, required.Roots...)
		} else if err != nil {
			return nil, err
		}
	}
	if len(checkins) == 0 {
		return nil, nil
	}
	return content.MissingAtHorizon(q, checkins, nil)
}

// missingOr is the refusal when the read's failure is content missing below
// the horizon, else the read's own error: a classification that cannot
// decide leaves that error as the diagnostic.
func missingOr(missing *content.MissingBelowHorizon, classifyErr, readErr error) error {
	if readErr == nil {
		panic("manifest.missingOr: no read error to classify")
	}
	if classifyErr == nil && missing != nil {
		return missing
	}
	return readErr
}

// Placement places the store cone's horizon once for an operation whose
// reads may each need to know whether a check-in lies below it: the held set
// is computed over the whole check-in graph, so one operation computes it at
// most once. A Placement belongs to one operation and is not shared.
type Placement struct {
	placed bool
	below  bool // false when the cone has no horizon the store can place
	k      []libfossil.FslID
}

// Below reports whether rid lies below the store cone's horizon: the cone
// has one, and rid is not in its held set K (draft-fossil-horizon-00 §3.1).
// False when the store cannot place C. A nil Placement places it afresh.
func (p *Placement) Below(ctx context.Context, q db.Querier, rid libfossil.FslID) (bool, error) {
	if ctx == nil || q == nil {
		panic("manifest.Placement.Below: nil context or querier")
	}
	if p == nil {
		p = &Placement{}
	}
	if !p.placed {
		if err := p.place(ctx, q); err != nil {
			return false, err
		}
	}
	if !p.below {
		return false, nil
	}
	_, held := slices.BinarySearch(p.k, rid)
	return !held, nil
}

func (p *Placement) place(ctx context.Context, q db.Querier) error {
	c, err := content.StoreCone(q)
	if err != nil {
		return err
	}
	p.placed = true
	if c == nil {
		return nil
	}
	h, ok := c.Horizon()
	if !ok {
		return nil
	}
	k, placed, err := horizon.Place(ctx, q, h)
	if err != nil {
		p.placed = false
		return err
	}
	p.k, p.below = k, placed
	return nil
}
