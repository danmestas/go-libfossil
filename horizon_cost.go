package libfossil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// HorizonRuleCost is what one rule would hold for a horizon at a check-in
// (draft-fossil-horizon-00 §3.4): the check-ins of the held set K and the
// blobs those check-ins' F cards name, the contents a clone at that horizon
// keeps.
type HorizonRuleCost struct {
	Rule          HorizonRule
	Checkins      int   // check-ins in K
	Blobs         int   // distinct blobs K's check-ins name, held here
	StoredBytes   int64 // what those blobs occupy in the repository, compressed and delta-encoded
	ExpandedBytes int64 // their sizes once expanded
}

// HorizonCost reports, for each rule in order (descendants, leaves), what a
// horizon at checkin would hold, so the choice of rule is made with its cost
// in view (§3.4). checkin is any name ResolveVersion accepts. The figures are
// read from this repository, which must hold the history: a blob it does not
// hold is left out of the byte counts, and a sub-manifest it lacks is an
// error.
func (r *Repo) HorizonCost(checkin string) ([]HorizonRuleCost, error) {
	rid, err := r.ResolveVersion(checkin)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	rules := []HorizonRule{HorizonDescendants, HorizonLeaves}
	out := make([]HorizonRuleCost, 0, len(rules))
	for _, rule := range rules {
		k, err := horizon.HeldSet(ctx, r.inner.DB(), libfossil.FslID(rid), cone.HorizonRule(rule))
		if err != nil {
			return nil, fmt.Errorf("libfossil: horizon cost: %w", err)
		}
		cost := HorizonRuleCost{Rule: rule, Checkins: len(k)}
		seen := map[string]bool{}
		for _, c := range k {
			files, err := manifest.ListFiles(r.inner, c)
			if err != nil {
				return nil, fmt.Errorf("libfossil: horizon cost: %w", err)
			}
			for _, f := range files {
				if seen[f.UUID] {
					continue
				}
				seen[f.UUID] = true
				var rid, size int64
				err := r.inner.DB().QueryRow(
					"SELECT rid, size FROM blob WHERE uuid=? AND size>=0", f.UUID,
				).Scan(&rid, &size)
				if errors.Is(err, sql.ErrNoRows) {
					continue // a phantom or an elided blob: not held, so not counted
				}
				if err != nil {
					return nil, fmt.Errorf("libfossil: horizon cost: blob %s: %w", f.UUID, err)
				}
				stored, _, err := blob.StoredHead(r.inner.DB(), libfossil.FslID(rid), 0)
				if errors.Is(err, blob.ErrNotStored) {
					stored = 0 // a row without content holds no stored bytes
				} else if err != nil {
					return nil, fmt.Errorf("libfossil: horizon cost: blob %s: %w", f.UUID, err)
				}
				cost.Blobs++
				cost.ExpandedBytes += size
				cost.StoredBytes += stored
			}
		}
		out = append(out, cost)
	}
	return out, nil
}
