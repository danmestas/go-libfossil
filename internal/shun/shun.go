package shun

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// Entry represents a shunned artifact.
type Entry struct {
	UUID    string
	MTime   int64
	Comment string
}

// Add marks an artifact as shunned. Idempotent — re-shunning updates
// mtime and comment. Returns error if uuid format is invalid.
func Add(q db.Querier, uuid, comment string) error {
	if q == nil {
		panic("shun.Add: q must not be nil")
	}
	if uuid == "" || !hash.IsValidHash(uuid) {
		return fmt.Errorf("shun.Add: invalid UUID %q", uuid)
	}
	_, err := q.Exec(
		"INSERT OR REPLACE INTO shun(uuid, mtime, scom) VALUES(?, strftime('%s','now'), ?)",
		uuid, comment,
	)
	return err
}

// Remove unshuns an artifact. No-op if uuid is not shunned.
func Remove(q db.Querier, uuid string) error {
	if q == nil {
		panic("shun.Remove: q must not be nil")
	}
	_, err := q.Exec("DELETE FROM shun WHERE uuid=?", uuid)
	return err
}

// IsShunned returns true if the given uuid is in the shun table.
func IsShunned(q db.Querier, uuid string) (bool, error) {
	if q == nil {
		panic("shun.IsShunned: q must not be nil")
	}
	var n int
	err := q.QueryRow("SELECT 1 FROM shun WHERE uuid=?", uuid).Scan(&n)
	if err != nil {
		return false, nil // sql.ErrNoRows or real error — treat both as "not shunned"
	}
	return true, nil
}

// List returns all shunned artifacts ordered by mtime descending.
func List(q db.Querier) ([]Entry, error) {
	if q == nil {
		panic("shun.List: q must not be nil")
	}
	rows, err := q.Query("SELECT uuid, CAST(mtime AS INTEGER), COALESCE(scom,'') FROM shun ORDER BY mtime DESC")
	if err != nil {
		return nil, fmt.Errorf("shun.List: %w", err)
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.UUID, &e.MTime, &e.Comment); err != nil {
			return nil, fmt.Errorf("shun.List scan: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
