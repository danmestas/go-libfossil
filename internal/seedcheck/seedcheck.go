// Package seedcheck keeps an origin from linking or serving a pushed horizon
// seed (draft-fossil-horizon-00 §5.2.3, as zit 0.7.1's erratum amends it).
//
// A pushed artifact is judged when it first becomes readable. The push
// handler judges what it can read at once; a delta it cannot read yet it
// records as pending here. Every path that fills a basis judges the pending
// rows it made readable in the same transaction (JudgeReadable): the push
// and pull cascades, a check-in, a tag, the sweep and a rebuild; and the
// crosslink step judges a pending row before it links it (Judge). A seed is
// marked refused; anything else loses its row. Until a row is cleared, its
// artifact is withheld: never announced or sent, pending or refused. Only
// pushed artifacts are recorded, since a seed-shaped artifact that arrives
// by any other route is ordinary.
//
// The table is store-local: it is in no configuration group, so no peer
// pulls it, and a refusal can never make a clone drop its own seed. It is
// created on the first push that needs it; a store without it withholds
// nothing.
package seedcheck

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
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
// arrived (refused 0, pending) or was found to be a seed (refused 1).
const table = "seedcheck"

const schema = `CREATE TABLE IF NOT EXISTS ` + table + `(
  rid INTEGER PRIMARY KEY,
  refused INTEGER NOT NULL
)`

// judgeCacheBytes bounds the expanded content JudgeReadable keeps while it
// walks the pending rows, so a pending chain is rebuilt once along its
// length.
const judgeCacheBytes = 16 << 20

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
// reports whether it is refused. The caller has seen the table exist
// (Exists), which it may cache for a transaction. A row not recorded is
// never refused.
func Judge(q db.Querier, rid libfossil.FslID, d *deck.Deck) (bool, error) {
	if q == nil || rid <= 0 || d == nil {
		panic("seedcheck.Judge: q and d must not be nil and rid must be positive")
	}
	refused, recorded, err := row(q, rid)
	if err != nil || !recorded || refused {
		return refused, err
	}
	return decide(q, rid, IsSeed(d))
}

// JudgeReadable judges every pending row whose content is now readable, in
// rid order through one cache: a seed becomes refused, anything else, files
// and bytes that hold no artifact included, loses its row. A row whose bytes
// do not hash to its name is not the artifact it names; it stays pending,
// withheld, rather than be judged on bytes that are not its own. Every path
// that fills a basis calls this in the transaction that filled it.
func JudgeReadable(q db.Querier) error {
	if q == nil {
		panic("seedcheck.JudgeReadable: q must not be nil")
	}
	ok, err := Exists(q)
	if err != nil || !ok {
		return err
	}
	pending, err := rids(q, "SELECT rid FROM "+table+" WHERE refused=0 ORDER BY rid")
	if err != nil {
		return err
	}
	cache := content.NewCache(judgeCacheBytes)
	for _, rid := range pending {
		if !content.IsAvailable(q, rid) {
			continue
		}
		data, err := cache.Expand(q, rid)
		if errors.Is(err, content.ErrHashMismatch) {
			continue
		}
		if err != nil {
			return fmt.Errorf("seedcheck.JudgeReadable: rid=%d: %w", rid, err)
		}
		d, perr := deck.Parse(data)
		if _, err := decide(q, rid, perr == nil && IsSeed(d)); err != nil {
			return err
		}
	}
	return nil
}

// decide records the verdict on pending rid. A seed becomes refused and
// leaves the unclustered table, so no inventory or cluster names it (a
// pushed artifact is never in unsent, which only local writes fill);
// anything else loses its row.
func decide(q db.Querier, rid libfossil.FslID, seed bool) (bool, error) {
	stmts := []string{"DELETE FROM " + table + " WHERE rid=?"}
	if seed {
		stmts = []string{
			"UPDATE " + table + " SET refused=1 WHERE rid=?",
			"DELETE FROM unclustered WHERE rid=?",
		}
	}
	for _, stmt := range stmts {
		if _, err := q.Exec(stmt, rid); err != nil {
			return false, fmt.Errorf("seedcheck: judge rid=%d: %w", rid, err)
		}
	}
	return seed, nil
}

// Clear drops rid's pending row: it was judged and is not a seed.
func Clear(q db.Querier, rid libfossil.FslID) error {
	if q == nil || rid <= 0 {
		panic("seedcheck.Clear: q must not be nil and rid must be positive")
	}
	ok, err := Exists(q)
	if err != nil || !ok {
		return err
	}
	if _, err := q.Exec("DELETE FROM "+table+" WHERE rid=? AND refused=0", rid); err != nil {
		return fmt.Errorf("seedcheck.Clear: %w", err)
	}
	return nil
}

// ForgetDeleted drops the rows whose blob rows are gone. A caller that
// deletes blob rows runs it in the same transaction: rids are reused, so a
// later artifact stored under one must inherit no verdict.
func ForgetDeleted(q db.Querier) error {
	if q == nil {
		panic("seedcheck.ForgetDeleted: q must not be nil")
	}
	ok, err := Exists(q)
	if err != nil || !ok {
		return err
	}
	if _, err := q.Exec("DELETE FROM " + table + " WHERE NOT EXISTS (SELECT 1 FROM blob WHERE rid=" +
		table + ".rid)"); err != nil {
		return fmt.Errorf("seedcheck.ForgetDeleted: %w", err)
	}
	return nil
}

// IsRefused reports whether rid was judged a seed.
func IsRefused(q db.Querier, rid libfossil.FslID) (bool, error) {
	if q == nil {
		panic("seedcheck.IsRefused: q must not be nil")
	}
	ok, err := Exists(q)
	if err != nil || !ok {
		return false, err
	}
	refused, _, err := row(q, rid)
	return refused, err
}

// IsWithheld reports whether rid has a row, pending or refused: a pushed
// artifact not yet judged, or judged a seed. No reply announces or sends it.
func IsWithheld(q db.Querier, rid libfossil.FslID) (bool, error) {
	if q == nil {
		panic("seedcheck.IsWithheld: q must not be nil")
	}
	ok, err := Exists(q)
	if err != nil || !ok {
		return false, err
	}
	_, recorded, err := row(q, rid)
	return recorded, err
}

// Withheld returns every rid IsWithheld reports. The set is small: it holds
// only pushed artifacts not yet readable, and pushed seeds.
func Withheld(q db.Querier) (map[libfossil.FslID]bool, error) {
	if q == nil {
		panic("seedcheck.Withheld: q must not be nil")
	}
	out := map[libfossil.FslID]bool{}
	ok, err := Exists(q)
	if err != nil || !ok {
		return out, err
	}
	all, err := rids(q, "SELECT rid FROM "+table)
	if err != nil {
		return nil, err
	}
	for _, rid := range all {
		out[rid] = true
	}
	return out, nil
}

// Exists reports whether the table was ever created: a store no unreadable
// delta was pushed to has none.
func Exists(q db.Querier) (bool, error) {
	if q == nil {
		panic("seedcheck.Exists: q must not be nil")
	}
	var n int
	if err := q.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).
		Scan(&n); err != nil {
		return false, fmt.Errorf("seedcheck: %w", err)
	}
	return n > 0, nil
}

// row reads rid's row in a table known to exist: whether it is refused, and
// whether it has one.
func row(q db.Querier, rid libfossil.FslID) (refused, recorded bool, err error) {
	var n int
	err = q.QueryRow("SELECT refused FROM "+table+" WHERE rid=?", rid).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("seedcheck: row of rid=%d: %w", rid, err)
	}
	return n == 1, true, nil
}

func rids(q db.Querier, query string) ([]libfossil.FslID, error) {
	rows, err := q.Query(query)
	if err != nil {
		return nil, fmt.Errorf("seedcheck: %w", err)
	}
	defer rows.Close()
	var out []libfossil.FslID
	for rows.Next() {
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return nil, fmt.Errorf("seedcheck: %w", err)
		}
		out = append(out, rid)
	}
	return out, rows.Err()
}
