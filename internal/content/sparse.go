package content

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// The store cone of draft-fossil-sparse-cone-00 §5 lives in a table of its
// own, created when a cone is set and dropped when it is cleared, so a dense
// repository's schema is untouched. `fossil rebuild` drops every table it
// does not know, which puts the cone in the disposable tier §5.2 recommends:
// losing it reclassifies every elided phantom as a transit phantom, and the
// store heals toward dense through ordinary synchronization.
const storeConeSchema = `CREATE TABLE IF NOT EXISTS storecone(
  id INTEGER PRIMARY KEY CHECK(id=1),
  cone BLOB NOT NULL
)`

// StoreCone returns the store cone, or nil when the store is dense. A cone
// this implementation cannot read -- a later serialization version, or bytes
// that do not parse -- is policy loss (§3.1, §5.2) and reads as dense, never
// as an error: the cost is bandwidth, not data.
func StoreCone(q db.Querier) (*cone.Cone, error) {
	if q == nil {
		panic("content.StoreCone: q must not be nil")
	}
	var n int
	if err := q.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='storecone'",
	).Scan(&n); err != nil {
		return nil, fmt.Errorf("content.StoreCone: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	var data []byte
	err := q.QueryRow("SELECT cone FROM storecone WHERE id=1").Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("content.StoreCone: %w", err)
	}
	c, err := cone.Parse(data)
	if err != nil {
		return nil, nil
	}
	return c, nil
}

// SetStoreCone replaces the store cone; nil makes the store dense. Neither
// direction moves content: narrowing deletes nothing (§5.4), and widening
// only makes the newly covered elided phantoms wanted again, for the next
// synchronization to fetch.
func SetStoreCone(q db.Querier, c *cone.Cone) error {
	if q == nil {
		panic("content.SetStoreCone: q must not be nil")
	}
	if c == nil {
		if _, err := q.Exec("DROP TABLE IF EXISTS storecone"); err != nil {
			return fmt.Errorf("content.SetStoreCone: %w", err)
		}
		return nil
	}
	if _, err := q.Exec(storeConeSchema); err != nil {
		return fmt.Errorf("content.SetStoreCone: %w", err)
	}
	if _, err := q.Exec("REPLACE INTO storecone(id, cone) VALUES(1, ?)", c.Serialize()); err != nil {
		return fmt.Errorf("content.SetStoreCone: %w", err)
	}
	return nil
}

// Absence classifies an artifact the store names: held, or absent for one
// of the three reasons of §5.2.
type Absence int

const (
	// Held is not absent: the store holds the content.
	Held Absence = iota
	// Transit is wanted and not yet arrived; synchronization requests it.
	Transit
	// Shunned is forbidden and never requested.
	Shunned
	// Elided is deliberately absent under the store cone. Synchronization
	// does not request it, and its absence is healthy.
	Elided
)

func (a Absence) String() string {
	switch a {
	case Held:
		return "held"
	case Transit:
		return "transit"
	case Shunned:
		return "shunned"
	case Elided:
		return "elided"
	}
	return fmt.Sprintf("Absence(%d)", int(a))
}

// Absences is the classification of every phantom in a store, computed from
// the store cone at load time. Elision is never stored on a phantom (§5.2):
// a cone change is a reload, not a migration.
type Absences struct {
	cone   *cone.Cone
	class  map[libfossil.FslID]Absence
	counts [Elided + 1]int
}

// LoadAbsences classifies the store's phantoms. A phantom is shunned when its
// name is in the shun table. Otherwise it is elided when the store has a
// cone, some F card names it under a path the cone does not cover, no F card
// names it under a covered path, and no held artifact is a delta over it;
// anything else is in transit. These are zit's rules (src/sparse.zig): only a
// file name proves an artifact out of cone, so a manifest, a sub-manifest
// node or a control artifact is never elided, and the store holds them
// densely (§5.1). F-card references are read from mlink, which records every
// (name, content) pair a check-in introduces, a rename's prior name included.
func LoadAbsences(q db.Querier) (*Absences, error) {
	if q == nil {
		panic("content.LoadAbsences: q must not be nil")
	}
	c, err := StoreCone(q)
	if err != nil {
		return nil, err
	}
	a := &Absences{cone: c, class: map[libfossil.FslID]Absence{}}
	rows, err := q.Query(`
		SELECT p.rid,
		       EXISTS(SELECT 1 FROM shun WHERE uuid=b.uuid),
		       EXISTS(SELECT 1 FROM delta d JOIN blob h ON h.rid=d.rid
		              WHERE d.srcid=p.rid AND h.size>=0)
		FROM phantom p JOIN blob b ON b.rid=p.rid`)
	if err != nil {
		return nil, fmt.Errorf("content.LoadAbsences: phantoms: %w", err)
	}
	candidates := map[libfossil.FslID]bool{}
	for rows.Next() {
		var rid libfossil.FslID
		var shunned, base bool
		if err := rows.Scan(&rid, &shunned, &base); err != nil {
			rows.Close()
			return nil, fmt.Errorf("content.LoadAbsences: phantoms: %w", err)
		}
		switch {
		case shunned:
			a.class[rid] = Shunned
		default:
			a.class[rid] = Transit
			if c != nil && !base {
				candidates[rid] = true
			}
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("content.LoadAbsences: phantoms: %w", err)
	}
	if len(candidates) > 0 {
		if err := a.elide(q, candidates); err != nil {
			return nil, err
		}
	}
	for _, k := range a.class {
		a.counts[k]++
	}
	return a, nil
}

// elide marks the candidates F cards name only outside the cone.
func (a *Absences) elide(q db.Querier, candidates map[libfossil.FslID]bool) error {
	rows, err := q.Query(`
		SELECT m.fid, f.name FROM phantom p
		  JOIN mlink m ON m.fid=p.rid JOIN filename f ON f.fnid=m.fnid
		UNION
		SELECT m.pid, f.name FROM phantom p
		  JOIN mlink m ON m.pid=p.rid
		  JOIN filename f ON f.fnid=CASE WHEN m.pfnid>0 THEN m.pfnid ELSE m.fnid END`)
	if err != nil {
		return fmt.Errorf("content.LoadAbsences: references: %w", err)
	}
	out := map[libfossil.FslID]bool{}
	in := map[libfossil.FslID]bool{}
	for rows.Next() {
		var rid libfossil.FslID
		var name string
		if err := rows.Scan(&rid, &name); err != nil {
			rows.Close()
			return fmt.Errorf("content.LoadAbsences: references: %w", err)
		}
		if !candidates[rid] {
			continue
		}
		if a.cone.Covers(name) {
			in[rid] = true
		} else {
			out[rid] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("content.LoadAbsences: references: %w", err)
	}
	for rid := range out {
		if !in[rid] {
			a.class[rid] = Elided
		}
	}
	return nil
}

// Cone is the store cone the classification used, nil for a dense store.
func (a *Absences) Cone() *cone.Cone { return a.cone }

// Class reports why rid is absent, or Held when it is not a phantom.
func (a *Absences) Class(rid libfossil.FslID) Absence {
	if k, ok := a.class[rid]; ok {
		return k
	}
	return Held
}

// Count is the number of phantoms in class k. Held is not counted.
func (a *Absences) Count(k Absence) int {
	if k == Held {
		panic("content.Absences.Count: held artifacts are not counted")
	}
	return a.counts[k]
}
