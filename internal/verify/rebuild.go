package verify

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// crosslinkTables are the derived tables a crosslink sweep owns. Rebuild empties
// them so every blob is a candidate again, then lets manifest.Crosslink refill
// them exactly as a clone that received every artifact would. This is the same
// set the repository-state corpus test empties before its sweep, which matches
// `fossil rebuild` in event, plink, mlink, tagxref, leaf and phantom.
var crosslinkTables = []string{
	"event", "plink", "leaf", "mlink", "tagxref",
	"forumpost", "attachment", "backlink", "cherrypick",
}

// bookkeepingTables are the sync-bookkeeping tables the crosslink sweep does not
// own. fossil's rebuild repopulates them; Rebuild clears and refills them after
// the sweep so a rebuilt repository offers every artifact to its next push.
var bookkeepingTables = []string{"unclustered", "unsent"}

// rebuildTableList reports every table Rebuild reconstructs, for Report.
var rebuildTableList = append(append([]string{}, crosslinkTables...), bookkeepingTables...)

// Rebuild reconstructs all derived tables from raw blob content. It first
// verifies blob integrity (read-only), then empties the crosslink-derived
// tables and runs the same crosslink sweep a clone runs, so derived state has a
// single owner -- manifest.Crosslink -- rather than a second, partial
// derivation that diverged from `fossil rebuild`. Finally it refills the
// sync-bookkeeping tables the sweep does not own.
func Rebuild(r *repo.Repo) (*Report, error) {
	if r == nil {
		panic("verify.Rebuild: nil *repo.Repo")
	}

	start := time.Now()
	report := &Report{}

	// Phase 1: verify blobs (read-only, outside any transaction). This is the
	// responsibility the crosslink sweep does not carry: every blob is expanded
	// and its content hashed against its stored UUID, so a corrupt or
	// delta-dangling repository is reported before any derived row is rewritten.
	cache := content.NewCache(verifyCacheBytes)
	if err := checkBlobs(r, report, cache); err != nil {
		return nil, fmt.Errorf("verify.Rebuild: %w", err)
	}

	// Phase 2: empty the crosslink-derived tables so every blob owes the sweep
	// an examination again. The deletes commit before the sweep because
	// manifest.Crosslink discovers candidates with a read outside any
	// transaction.
	if err := r.WithTx(func(tx *db.Tx) error {
		return clearTables(tx, crosslinkTables)
	}); err != nil {
		return nil, fmt.Errorf("verify.Rebuild: %w", err)
	}

	// Phase 3: crosslink every artifact. The sweep writes event/plink/mlink/
	// tagxref, creates phantoms for absent targets, parents and file content,
	// reparents, then repairs leaf and tag-propagation state over the whole plink
	// graph -- the order `fossil rebuild` uses. CrosslinkRebuild, not Crosslink,
	// because a rebuild is terminal: a check-in whose content is absent is linked
	// with phantoms rather than deferred for a blob that will never arrive.
	if _, err := manifest.CrosslinkRebuild(r); err != nil {
		return nil, fmt.Errorf("verify.Rebuild: %w", err)
	}

	// Phase 4: refill the sync-bookkeeping tables the sweep does not own.
	if err := r.WithTx(rebuildBookkeeping); err != nil {
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

// clearTables deletes all rows from the named tables that exist. A table may be
// absent: a repository created by fossil(1) without the feature that owns it
// (forumpost until the first forum post, say) has no such table until the
// crosslink sweep's prepareCrosslinkTables creates it, and there is nothing to
// clear in that case.
func clearTables(tx *db.Tx, tables []string) error {
	if tx == nil {
		panic("verify.clearTables: nil *db.Tx")
	}
	for _, table := range tables {
		exists, err := tableExists(tx, table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	return nil
}

// tableExists reports whether the named table is present in the schema.
func tableExists(tx *db.Tx, table string) (bool, error) {
	var name string
	err := tx.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("table exists %s: %w", table, err)
	}
	return true, nil
}

// rebuildBookkeeping repopulates unclustered and unsent. Every non-phantom blob
// is marked unclustered and unsent so sync re-evaluates what to push. It clears
// first so a repeated Rebuild stays idempotent.
func rebuildBookkeeping(tx *db.Tx) error {
	if tx == nil {
		panic("verify.rebuildBookkeeping: nil *db.Tx")
	}
	if err := clearTables(tx, bookkeepingTables); err != nil {
		return err
	}
	for _, table := range bookkeepingTables {
		_, err := tx.Exec("INSERT INTO " + table + "(rid) SELECT rid FROM blob WHERE size >= 0")
		if err != nil {
			return fmt.Errorf("rebuildBookkeeping %s: %w", table, err)
		}
	}
	return nil
}
