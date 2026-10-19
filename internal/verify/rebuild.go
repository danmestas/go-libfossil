package verify

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// crosslinkTables are the derived tables a crosslink sweep owns. Rebuild empties
// them so every blob is a candidate again, then lets manifest.Crosslink refill
// them exactly as a clone that received every artifact would. This is the same
// set the repository-state corpus test empties before its sweep, which matches
// `fossil rebuild` in event, plink, mlink, tagxref, leaf and phantom. orphan is
// among them because the sweep files a delta manifest whose baseline is absent
// there; `fossil rebuild` drops and recreates it the same way.
var crosslinkTables = []string{
	"event", "plink", "leaf", "mlink", "tagxref",
	"forumpost", "attachment", "backlink", "cherrypick", "orphan",
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
// sync-bookkeeping tables the sweep does not own. It holds one write
// transaction for the whole sweep, so a concurrent writer waits up to
// busy_timeout and then gets SQLITE_BUSY, as with fossil's rebuild.
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
	if err := checkBlobs(r, report); err != nil {
		return nil, fmt.Errorf("verify.Rebuild: %w", err)
	}

	// Everything after the read-only check runs in one transaction, as
	// fossil's rebuild_db does, so a sweep that fails partway leaves the
	// derived state Rebuild found rather than emptied tables.
	if err := r.WithTx(rederive); err != nil {
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

// rederive empties the crosslink-derived tables so every blob owes the sweep an
// examination again, crosslinks every artifact, and refills the sync-bookkeeping
// tables the sweep does not own. The sweep writes event/plink/mlink/tagxref,
// creates phantoms for absent targets, parents and file content, reparents,
// then repairs leaf and tag-propagation state over the whole plink graph -- the
// order `fossil rebuild` uses. CrosslinkRebuild, not Crosslink, so the whole
// rebuild runs in tx: a check-in whose files or parent are absent is linked
// with phantoms, and a delta manifest whose baseline is absent is refused and
// filed in orphan, exactly as Crosslink and `fossil rebuild` do.
func rederive(tx *db.Tx) error {
	if err := clearTables(tx, crosslinkTables); err != nil {
		return err
	}
	if _, err := manifest.CrosslinkRebuild(tx); err != nil {
		return err
	}
	return rebuildBookkeeping(tx)
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
