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
// Horizon (§5.5): Base is the boundary check-in the store's walk reached,
// "" when it reached none because both sides' ancestry leaves the store
// before they meet. Side is the check-in being merged in.
type MergeBaseError struct {
	Horizon, Side, Base string
}

func (e *MergeBaseError) Error() string {
	if e.Base != "" {
		return fmt.Sprintf("the merge base of %s is %s, below the structural horizon at %s: it is not held here; "+
			"deepen the horizon to it (repo cone deepen %s), or merge with a remote to ask the origin and deepen",
			e.Side, e.Base, e.Horizon, e.Base)
	}
	return fmt.Sprintf("the merge base of %s lies below the structural horizon at %s: it is not held here; "+
		"merge with a remote to ask the origin for the base and deepen to it", e.Side, e.Horizon)
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

// MergeBaseCrossing decides whether the merge of side into current crosses
// the store's structural horizon (§5.5), given the common ancestor base the
// store's own walk found, 0 for none. A base the store does not hold as a
// check-in is a crossing; so is no base at all when both sides' ancestry
// reaches a parent the store does not hold, since the common ancestor may
// lie there. It returns a *MergeBaseError for a crossing and nil otherwise,
// so a merge never runs against a base it does not hold.
func MergeBaseCrossing(q db.Querier, current, side, base libfossil.FslID) error {
	if q == nil {
		panic("content.MergeBaseCrossing: q must not be nil")
	}
	if current <= 0 || side <= 0 || base < 0 {
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
		if err := q.QueryRow(`SELECT size>=0 AND EXISTS(SELECT 1 FROM event WHERE objid=rid AND type='ci')
			AND NOT EXISTS(SELECT 1 FROM phantom WHERE phantom.rid=blob.rid), uuid
			FROM blob WHERE rid=?`, base).Scan(&held, &crossing.Base); err != nil {
			return fmt.Errorf("content.MergeBaseCrossing: %w", err)
		}
		if held {
			return nil
		}
		return crossing
	}
	for _, rid := range []libfossil.FslID{current, side} {
		leaves, err := ancestryLeavesStore(q, rid)
		if err != nil || !leaves {
			return err
		}
	}
	return crossing
}

// ancestryLeavesStore reports a parent the store does not hold as a check-in
// somewhere in rid's ancestry. The recursion stops at each such parent, which
// names no parent the store links, so it visits each held ancestor once.
func ancestryLeavesStore(q db.Querier, rid libfossil.FslID) (bool, error) {
	var leaves bool
	err := q.QueryRow(`WITH RECURSIVE anc(rid) AS (
			SELECT ?1 UNION SELECT plink.pid FROM plink JOIN anc ON plink.cid=anc.rid)
		SELECT EXISTS(SELECT 1 FROM anc JOIN blob USING(rid) WHERE blob.size<0
			OR NOT EXISTS(SELECT 1 FROM event WHERE objid=anc.rid AND type='ci'))`, rid).Scan(&leaves)
	if err != nil {
		return false, fmt.Errorf("content.MergeBaseCrossing: ancestry: %w", err)
	}
	return leaves, nil
}
