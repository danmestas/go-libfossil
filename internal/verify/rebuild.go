package verify

import (
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// rebuildTableList is the set of derived tables dropped and reconstructed
// during a rebuild. Order does not matter — they are all DELETEd first.
var rebuildTableList = []string{
	"event", "mlink", "plink", "tagxref", "filename",
	"leaf", "unclustered", "unsent",
}

// Rebuild reconstructs all derived tables from raw blob content.
// It first verifies blob integrity (read-only), then drops and
// rebuilds event/mlink/plink/tagxref/filename/leaf/unclustered/unsent
// in a single transaction.
func Rebuild(r *repo.Repo) (*Report, error) {
	if r == nil {
		panic("verify.Rebuild: nil *repo.Repo")
	}

	start := time.Now()
	report := &Report{}

	// One expansion cache for the whole rebuild; see verifyCacheBytes. It spans
	// the pre-transaction blob check and the in-transaction sweeps because a
	// blob's rid and content are the same whichever querier reads them, and
	// rebuild never rewrites blob content -- only the derived tables.
	cache := content.NewCache(verifyCacheBytes)

	// Phase 1: verify blobs (read-only, outside transaction)
	if err := checkBlobs(r, report, cache); err != nil {
		return nil, fmt.Errorf("verify.Rebuild: %w", err)
	}

	// Phases 2-4: drop + reconstruct in transaction
	if err := r.WithTx(func(tx *db.Tx) error {
		if err := dropDerivedTables(tx); err != nil {
			return err
		}
		if err := rebuildManifests(r, tx, report, cache); err != nil {
			return fmt.Errorf("rebuild manifests: %w", err)
		}
		if err := rebuildTags(r, tx, report, cache); err != nil {
			return fmt.Errorf("rebuild tags: %w", err)
		}
		if err := rebuildLeaves(tx); err != nil {
			return fmt.Errorf("rebuild leaves: %w", err)
		}
		if err := rebuildBookkeeping(tx); err != nil {
			return fmt.Errorf("rebuild bookkeeping: %w", err)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("verify.Rebuild: %w", err)
	}

	report.TablesRebuilt = rebuildTableList
	report.Duration = time.Since(start)

	// Postcondition: TablesRebuilt must be populated after successful rebuild.
	if len(report.TablesRebuilt) == 0 {
		panic("verify.Rebuild: postcondition: TablesRebuilt empty after successful rebuild")
	}

	return report, nil
}

// dropDerivedTables deletes all rows from the derived tables.
func dropDerivedTables(tx *db.Tx) error {
	if tx == nil {
		panic("verify.dropDerivedTables: nil *db.Tx")
	}
	for _, table := range rebuildTableList {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return fmt.Errorf("drop %s: %w", table, err)
		}
	}
	return nil
}
