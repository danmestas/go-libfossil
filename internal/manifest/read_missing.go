package manifest

import (
	"context"
	"errors"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// ReadFileAt reads filePath as check-in rid names it, serving the manifest and
// the payload through cache (nil reads uncached); found is false when rid
// does not name the file. When the read needs an artifact the store does not
// hold and the store cone has a horizon, it fails with a
// *content.MissingBelowHorizon naming the check-in, when its structure is
// what is missing, or else the payload (draft-fossil-horizon-00 §4.4).
func ReadFileAt(
	ctx context.Context, r *repo.Repo, rid libfossil.FslID, filePath string, cache *content.Cache,
) (data []byte, found bool, err error) {
	if ctx == nil || r == nil {
		panic("manifest.ReadFileAt: nil context or repo")
	}
	if filePath == "" {
		panic("manifest.ReadFileAt: filePath must not be empty")
	}
	f, found, err := LookupFileCached(r, rid, filePath, cache)
	if err != nil {
		missing, merr := MissingStructure(ctx, r.DB(), []libfossil.FslID{rid})
		return nil, false, missingOr(missing, merr, err)
	}
	if !found {
		return nil, false, nil
	}
	fileRID, err := content.RequireAvailableByUUID(r.DB(), f.UUID)
	if err != nil {
		missing, merr := content.MissingAtHorizon(r.DB(), nil, []string{f.UUID})
		return nil, false, missingOr(missing, merr, err)
	}
	data, err = cache.Expand(r.DB(), fileRID)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// MissingStructure is the refusal naming each check-in among rids whose
// manifest, baseline or nodes the store does not hold, when its cone has a
// horizon; nil when every one is held or the cone has none.
func MissingStructure(
	ctx context.Context, q db.Querier, rids []libfossil.FslID,
) (*content.MissingBelowHorizon, error) {
	if ctx == nil || q == nil {
		panic("manifest.MissingStructure: nil context or querier")
	}
	var checkins []string
	for _, rid := range rids {
		err := RequireParentContent(ctx, q, []libfossil.FslID{rid})
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
