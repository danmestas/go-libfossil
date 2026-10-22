package content

import (
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// belowLabelTable holds the boundary labels of a structural horizon
// (draft-fossil-horizon-00 §5.3, §6.2.2): the names an origin said lie below
// the horizon. They live beside the store cone, as store-adjacent state no
// peer is sent, and like it in a table of their own that `fossil rebuild`
// drops; losing them makes the boundary parents transit again, which costs a
// request the origin answers by labelling them anew.
const belowLabelTable = "horizonbelow"

const belowLabelSchema = `CREATE TABLE IF NOT EXISTS ` + belowLabelTable + `(
  uuid TEXT PRIMARY KEY
) WITHOUT ROWID`

// maxBelowLabels bounds the labels a store keeps. An origin labels the
// parents its held set names below it, so the bound is the held-set walk's.
const maxBelowLabels = coneReferenceNodes

// horizonOf is c's horizon member, the zero Horizon when it has none.
func horizonOf(c *cone.Cone) cone.Horizon {
	if c == nil {
		return cone.Horizon{}
	}
	h, _ := c.Horizon()
	return h
}

// LabelBelow records names an origin labelled below the store's structural
// horizon. A name that is no artifact hash is ignored, as a malformed pragma
// is; labelling is idempotent.
func LabelBelow(q db.Querier, names []string) error {
	if q == nil {
		panic("content.LabelBelow: q must not be nil")
	}
	if len(names) == 0 {
		return nil
	}
	if _, err := q.Exec(belowLabelSchema); err != nil {
		return fmt.Errorf("content.LabelBelow: %w", err)
	}
	var n int
	if err := q.QueryRow("SELECT count(*) FROM " + belowLabelTable).Scan(&n); err != nil {
		return fmt.Errorf("content.LabelBelow: %w", err)
	}
	for _, name := range names {
		if !hash.IsValidHash(name) {
			continue
		}
		if n >= maxBelowLabels {
			return fmt.Errorf("content.LabelBelow: more than %d labels", maxBelowLabels)
		}
		res, err := q.Exec("INSERT OR IGNORE INTO "+belowLabelTable+"(uuid) VALUES(?)", name)
		if err != nil {
			return fmt.Errorf("content.LabelBelow: %w", err)
		}
		if added, err := res.RowsAffected(); err == nil {
			n += int(added)
		}
	}
	return nil
}

// BelowLabels returns the boundary labels the store holds.
func BelowLabels(q db.Querier) (map[string]bool, error) {
	if q == nil {
		panic("content.BelowLabels: q must not be nil")
	}
	var n int
	if err := q.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", belowLabelTable,
	).Scan(&n); err != nil {
		return nil, fmt.Errorf("content.BelowLabels: %w", err)
	}
	out := map[string]bool{}
	if n == 0 {
		return out, nil
	}
	rows, err := q.Query("SELECT uuid FROM " + belowLabelTable)
	if err != nil {
		return nil, fmt.Errorf("content.BelowLabels: %w", err)
	}
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out[uuid] = true
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
