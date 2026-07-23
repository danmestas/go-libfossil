package search

import (
	"database/sql"
	"fmt"
	"strconv"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// trunkTip returns the RID of the latest checkin on trunk.
// Returns 0 if no trunk tip exists (empty repo).
func trunkTip(q db.Querier) (libfossil.FslID, error) {
	if q == nil {
		panic("search.trunkTip: nil Querier")
	}

	var rid int64
	err := q.QueryRow(`
		SELECT tagxref.rid
		FROM tagxref
		JOIN tag USING(tagid)
		WHERE tag.tagname = 'sym-trunk' AND tagxref.tagtype > 0
		ORDER BY tagxref.mtime DESC
		LIMIT 1
	`).Scan(&rid)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("trunkTip: %w", err)
	}
	if rid <= 0 {
		panic("search.trunkTip: postcondition: rid must be positive when row found")
	}

	return libfossil.FslID(rid), nil
}

// indexedRID returns the RID stored in fts_meta, or 0 if not set.
func indexedRID(q db.Querier) (libfossil.FslID, error) {
	var val string
	err := q.QueryRow(
		"SELECT value FROM fts_meta WHERE key = 'indexed_rid'",
	).Scan(&val)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("indexedRID: %w", err)
	}
	rid, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("indexedRID parse: %w", err)
	}
	return libfossil.FslID(rid), nil
}

// NeedsReindex returns true if the trunk tip has advanced past the indexed checkin.
// Returns false if the repo has no trunk tip (empty repo).
func (idx *Index) NeedsReindex() (bool, error) {
	if idx == nil {
		panic("search.NeedsReindex: nil *Index")
	}

	tip, err := trunkTip(idx.repo.DB())
	if err != nil {
		return false, fmt.Errorf("search.NeedsReindex: %w", err)
	}
	if tip == 0 {
		return false, nil // empty repo
	}

	current, err := indexedRID(idx.repo.DB())
	if err != nil {
		return false, fmt.Errorf("search.NeedsReindex: %w", err)
	}

	return tip != current, nil
}
