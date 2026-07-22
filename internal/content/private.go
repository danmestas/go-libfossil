package content

import (
	"github.com/danmestas/go-libfossil/db"
)

// IsPrivate returns true if the blob with the given rid is in the private table.
func IsPrivate(q db.Querier, rid int64) bool {
	if q == nil {
		panic("content.IsPrivate: q must not be nil")
	}
	if rid <= 0 {
		panic("content.IsPrivate: rid must be positive")
	}
	var n int
	err := q.QueryRow("SELECT 1 FROM private WHERE rid=?", rid).Scan(&n)
	return err == nil
}

// MakePrivate inserts the rid into the private table (no-op if already present).
func MakePrivate(q db.Querier, rid int64) error {
	if q == nil {
		panic("content.MakePrivate: q must not be nil")
	}
	if rid <= 0 {
		panic("content.MakePrivate: rid must be positive")
	}
	_, err := q.Exec("INSERT OR IGNORE INTO private(rid) VALUES(?)", rid)
	return err
}

// MakePublic removes the rid from the private table (no-op if not present).
func MakePublic(q db.Querier, rid int64) error {
	if q == nil {
		panic("content.MakePublic: q must not be nil")
	}
	if rid <= 0 {
		panic("content.MakePublic: rid must be positive")
	}
	_, err := q.Exec("DELETE FROM private WHERE rid=?", rid)
	return err
}
