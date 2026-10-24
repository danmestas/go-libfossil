// Package seedcheck keeps an origin from linking or serving a pushed horizon
// seed (draft-fossil-horizon-00 §5.2.3, as zit 0.7.1's erratum amends it).
//
// A pushed artifact is judged when it first becomes readable. The push
// handler judges what it can read at once; a delta it cannot read yet it
// records as pending here. Every path that later makes such a delta readable
// (a push or pull supplying its basis, a check-in writing the basis's bytes,
// a sweep, a rebuild) reaches the crosslink step, which judges a pending row
// before linking it: a seed is marked refused and never linked, announced or
// sent; anything else loses its row and links as usual. Only pushed
// artifacts are recorded, since a seed-shaped artifact that arrives by any
// other route is ordinary.
//
// The table is store-local: it is in no configuration group, so no peer
// pulls it, and a refusal can never make a clone drop its own seed.
package seedcheck

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// Label is the U card of the seeds go-libfossil mints: its own seed label,
// never a user's login, since a seed is a tag application no user made
// (§5.2.2).
const Label = "go-libfossil"

// Tag is the tag name of a seed's binding card; no pushed control artifact
// may carry it (§5.2.3).
const Tag = "horizon-seed"

// table holds one row per pushed artifact that was unreadable when it
// arrived (state pending) or was found to be a seed (state refused).
const table = "seedcheck"

const schema = `CREATE TABLE IF NOT EXISTS ` + table + `(
  rid INTEGER PRIMARY KEY,
  refused INTEGER NOT NULL
)`

// IsSeed reports whether d is a seed an origin refuses: a control artifact
// whose U card is Label, or one carrying a Tag T card under any label.
// Another implementation's label alone is ordinary, and so is a user's
// login unless it equals Label.
func IsSeed(d *deck.Deck) bool {
	if d == nil {
		panic("seedcheck.IsSeed: d must not be nil")
	}
	if d.Type != deck.Control {
		return false
	}
	if d.U != nil && *d.U == Label {
		return true
	}
	for _, t := range d.T {
		if t.Name == Tag {
			return true
		}
	}
	return false
}

// IsSeedBytes is IsSeed over an artifact's full content. Bytes that name
// neither the label nor the tag are rejected without parsing.
func IsSeedBytes(data []byte) bool {
	if data == nil {
		panic("seedcheck.IsSeedBytes: data must not be nil")
	}
	if !bytes.Contains(data, []byte("\nU "+Label+"\n")) && !bytes.Contains(data, []byte(Tag)) {
		return false
	}
	d, err := deck.Parse(data)
	return err == nil && IsSeed(d)
}

// MarkPending records rid, a pushed artifact not yet readable, for judging
// when it becomes readable.
func MarkPending(q db.Querier, rid libfossil.FslID) error {
	if q == nil || rid <= 0 {
		panic("seedcheck.MarkPending: q must not be nil and rid must be positive")
	}
	if _, err := q.Exec(schema); err != nil {
		return fmt.Errorf("seedcheck.MarkPending: %w", err)
	}
	if _, err := q.Exec("INSERT OR IGNORE INTO "+table+"(rid, refused) VALUES(?, 0)", rid); err != nil {
		return fmt.Errorf("seedcheck.MarkPending: %w", err)
	}
	return nil
}

// Judge decides rid, about to be linked with d its parsed content, and
// reports whether it is refused. A pending seed becomes refused and leaves
// the unclustered and unsent tables, so no inventory or cluster names it;
// any other pending row is cleared. A row not recorded is never refused.
func Judge(q db.Querier, rid libfossil.FslID, d *deck.Deck) (bool, error) {
	if q == nil || rid <= 0 || d == nil {
		panic("seedcheck.Judge: q and d must not be nil and rid must be positive")
	}
	refused, recorded, err := state(q, rid)
	if err != nil || !recorded || refused {
		return refused, err
	}
	if !IsSeed(d) {
		return false, Clear(q, rid)
	}
	return true, refuse(q, rid)
}

// Clear drops rid's pending row: it was judged and is not a seed.
func Clear(q db.Querier, rid libfossil.FslID) error {
	if q == nil || rid <= 0 {
		panic("seedcheck.Clear: q must not be nil and rid must be positive")
	}
	ok, err := exists(q)
	if err != nil || !ok {
		return err
	}
	if _, err := q.Exec("DELETE FROM "+table+" WHERE rid=? AND refused=0", rid); err != nil {
		return fmt.Errorf("seedcheck.Clear: %w", err)
	}
	return nil
}

func refuse(q db.Querier, rid libfossil.FslID) error {
	for _, stmt := range []string{
		"UPDATE " + table + " SET refused=1 WHERE rid=?",
		"DELETE FROM unclustered WHERE rid=?",
		"DELETE FROM unsent WHERE rid=?",
	} {
		if _, err := q.Exec(stmt, rid); err != nil {
			return fmt.Errorf("seedcheck: refuse rid=%d: %w", rid, err)
		}
	}
	return nil
}

// IsRefused reports whether rid was judged a seed.
func IsRefused(q db.Querier, rid libfossil.FslID) (bool, error) {
	if q == nil {
		panic("seedcheck.IsRefused: q must not be nil")
	}
	refused, _, err := state(q, rid)
	return refused, err
}

// Refused returns every rid judged a seed. The set is small: it holds only
// seeds pushed to this origin.
func Refused(q db.Querier) (map[libfossil.FslID]bool, error) {
	if q == nil {
		panic("seedcheck.Refused: q must not be nil")
	}
	out := map[libfossil.FslID]bool{}
	ok, err := exists(q)
	if err != nil || !ok {
		return out, err
	}
	rows, err := q.Query("SELECT rid FROM " + table + " WHERE refused=1")
	if err != nil {
		return nil, fmt.Errorf("seedcheck.Refused: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return nil, fmt.Errorf("seedcheck.Refused: %w", err)
		}
		out[rid] = true
	}
	return out, rows.Err()
}

// state reads rid's row: whether it is refused, and whether it has one.
func state(q db.Querier, rid libfossil.FslID) (refused, recorded bool, err error) {
	ok, err := exists(q)
	if err != nil || !ok {
		return false, false, err
	}
	var n int
	err = q.QueryRow("SELECT refused FROM "+table+" WHERE rid=?", rid).Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("seedcheck: state of rid=%d: %w", rid, err)
	}
	return n == 1, true, nil
}

// exists reports whether the table was ever created: a store no seed was
// pushed to has none.
func exists(q db.Querier) (bool, error) {
	var n int
	if err := q.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).
		Scan(&n); err != nil {
		return false, fmt.Errorf("seedcheck: %w", err)
	}
	return n > 0, nil
}
