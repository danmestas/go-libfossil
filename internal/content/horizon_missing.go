package content

import (
	"fmt"
	"slices"

	"github.com/danmestas/go-libfossil/db"
)

// DemandFetchLimit bounds the artifacts one targeted demand fetch asks a peer
// for: one exchange's gimmes (sync.MaxGimmeBase), so the fetch is one round.
// A read needing more is the case a deepen is for.
const DemandFetchLimit = 200

// MissingBelowHorizon is a read's refusal for artifacts the store does not
// hold under a store cone with a horizon at Horizon (draft-fossil-horizon-00
// §4.4). Checkins are check-ins whose manifests, baselines and nodes the read
// needs; Names are the file payloads. They are exactly what a targeted demand
// fetch asks a peer for, which leaves the horizon where it is; the other
// remedy is a deepen, which moves it.
type MissingBelowHorizon struct {
	Horizon  string
	Checkins []string
	Names    []string
}

func (e *MissingBelowHorizon) Error() string {
	first := ""
	if len(e.Checkins) > 0 {
		first = "check-in " + e.Checkins[0]
	} else if len(e.Names) > 0 {
		first = e.Names[0]
	}
	return fmt.Sprintf("%d artifact(s) the read needs, first %s, are not held by a store whose horizon is at %s; "+
		"fetch them from a peer (the read's Context form) or deepen the horizon (DeepenHorizon)",
		len(e.Checkins)+len(e.Names), first, e.Horizon)
}

func (e *MissingBelowHorizon) Is(target error) bool { return target == ErrBelowHorizon }

// Join is the refusal naming what e and o both name, either of which may be
// nil. A read that needs several artifacts joins its refusals so that one
// demand fetch asks for all of them.
func (e *MissingBelowHorizon) Join(o *MissingBelowHorizon) *MissingBelowHorizon {
	if e == nil {
		return o
	}
	if o == nil {
		return e
	}
	if e.Horizon != o.Horizon {
		panic("content.MissingBelowHorizon.Join: refusals under different horizons")
	}
	return &MissingBelowHorizon{
		Horizon:  e.Horizon,
		Checkins: unionNames(e.Checkins, o.Checkins),
		Names:    unionNames(e.Names, o.Names),
	}
}

func unionNames(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// MissingAtHorizon is the refusal naming checkins and names, none of which
// the store holds, when its cone has a horizon. It is nil when the store cone
// has none, or when one of them is shunned: no peer may supply that, so the
// caller keeps its own diagnostic.
func MissingAtHorizon(q db.Querier, checkins, names []string) (*MissingBelowHorizon, error) {
	if q == nil {
		panic("content.MissingAtHorizon: q must not be nil")
	}
	if len(checkins)+len(names) == 0 {
		panic("content.MissingAtHorizon: nothing is missing")
	}
	c, err := StoreCone(q)
	if err != nil {
		return nil, err
	}
	h := horizonOf(c).Hash()
	if h == "" {
		return nil, nil
	}
	for _, uuid := range slices.Concat(checkins, names) {
		var barred bool
		if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM shun WHERE uuid=?)", uuid).Scan(&barred); err != nil {
			return nil, err
		}
		if barred {
			return nil, nil
		}
	}
	return &MissingBelowHorizon{
		Horizon:  h,
		Checkins: unionNames(checkins, nil),
		Names:    unionNames(names, nil),
	}, nil
}
