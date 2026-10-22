package manifest

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
)

// rederivedTables are the derived tables a crosslink sweep owns. Rederive
// empties them so every blob is a candidate again, then lets CrosslinkRebuild
// refill them exactly as a clone that received every artifact would. This is
// the same set the repository-state corpus test empties before its sweep,
// which matches `fossil rebuild` in event, plink, mlink, tagxref, leaf and
// phantom. orphan is among them because the sweep files a delta manifest whose
// baseline is absent there; `fossil rebuild` drops and recreates it the same
// way.
var rederivedTables = []string{
	"event", "plink", "leaf", "mlink", "tagxref",
	"forumpost", "attachment", "backlink", "cherrypick", "orphan",
}

// RederivedTables returns the tables Rederive empties and refills.
func RederivedTables() []string {
	return append([]string{}, rederivedTables...)
}

// Rederive empties the crosslink-derived tables and crosslinks every artifact
// again, entirely within tx (CrosslinkRebuild), returning how many it linked.
// The sweep writes event/plink/mlink/tagxref, creates phantoms for absent
// targets, parents and file content, reparents, then repairs leaf and
// tag-propagation state over the whole plink graph -- the order `fossil
// rebuild` uses. A table may be absent: a repository created by fossil(1)
// without the feature that owns it (forumpost until the first forum post, say)
// has no such table until the sweep creates it, and there is nothing to clear.
func Rederive(tx *db.Tx) (int, error) {
	if tx == nil {
		panic("manifest.Rederive: tx must not be nil")
	}
	for _, table := range rederivedTables {
		var n int
		err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?",
			table).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("manifest.Rederive: table %s: %w", table, err)
		}
		if n == 0 {
			continue
		}
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return 0, fmt.Errorf("manifest.Rederive: clear %s: %w", table, err)
		}
	}
	return CrosslinkRebuild(tx)
}
