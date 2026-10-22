package content

import (
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ErrBelowHorizon is matched by the diagnostic of an operation that needs
// history below the store's structural horizon (draft-fossil-horizon-00
// §5.5): the store does not hold it, and a deepen is what brings it.
var ErrBelowHorizon = errors.New("history below the structural horizon")

// MergeBaseError is a merge whose base lies below the structural horizon at
// Horizon (§5.5), or may: Base is the base the store's walk found when the
// store does not hold it, "" when the walk crossed the horizon, so that only
// the origin can name the base. Side is the check-in being merged in.
type MergeBaseError struct {
	Horizon, Side, Base string
}

func (e *MergeBaseError) Error() string {
	if e.Base != "" {
		return fmt.Sprintf("the merge base of %s is %s, below the structural horizon at %s: it is not held here; "+
			"deepen the horizon to it (repo cone deepen %s), or merge with a remote to ask the origin and deepen",
			e.Side, e.Base, e.Horizon, e.Base)
	}
	return fmt.Sprintf("the merge base of %s may lie below the structural horizon at %s: its ancestry crosses the "+
		"horizon, so only the origin can name it; merge with a remote to ask the origin for the base and deepen to it",
		e.Side, e.Horizon)
}

func (e *MergeBaseError) Is(target error) bool { return target == ErrBelowHorizon }

// StructuralHorizon is the store cone's horizon when it is structural.
func StructuralHorizon(q db.Querier) (cone.Horizon, bool, error) {
	if q == nil {
		panic("content.StructuralHorizon: q must not be nil")
	}
	c, err := StoreCone(q)
	if err != nil {
		return cone.Horizon{}, false, err
	}
	h := horizonOf(c)
	return h, h.Hash() != "" && h.Depth() == cone.DepthStructural, nil
}

// MergeBaseCrossing decides whether the merge of side crosses the store's
// structural horizon (§5.5), given the base the store's own walk found, 0
// for none, and whether that walk reached a check-in the store does not
// hold (merge.Pivot.Unheld). A base the store does not hold is a crossing.
// So is any walk that reached the horizon: the store holds only part of the
// graph, and a common ancestor found in part of it need not be the one the
// whole graph gives, so the origin must be asked. It returns a
// *MergeBaseError for a crossing and nil otherwise, so a merge never runs
// against a base it does not hold or one the origin would not choose.
func MergeBaseCrossing(q db.Querier, side, base libfossil.FslID, unheld bool) error {
	if q == nil {
		panic("content.MergeBaseCrossing: q must not be nil")
	}
	if side <= 0 || base < 0 {
		panic("content.MergeBaseCrossing: invalid rid")
	}
	h, structural, err := StructuralHorizon(q)
	if err != nil || !structural {
		return err
	}
	crossing := &MergeBaseError{Horizon: h.Hash()}
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", side).Scan(&crossing.Side); err != nil {
		return fmt.Errorf("content.MergeBaseCrossing: %w", err)
	}
	if base > 0 {
		var held bool
		var uuid string
		if err := q.QueryRow(`SELECT size>=0 AND EXISTS(SELECT 1 FROM event WHERE objid=rid AND type='ci')
			AND NOT EXISTS(SELECT 1 FROM phantom WHERE phantom.rid=blob.rid), uuid
			FROM blob WHERE rid=?`, base).Scan(&held, &uuid); err != nil {
			return fmt.Errorf("content.MergeBaseCrossing: %w", err)
		}
		if !held {
			crossing.Base = uuid
			return crossing
		}
	}
	if unheld {
		return crossing
	}
	return nil
}
