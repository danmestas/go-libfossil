package verify

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/horizon/seed"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
)

// bookkeepingTables are the sync-bookkeeping tables the crosslink sweep does not
// own. fossil's rebuild repopulates them; Rebuild clears and refills them after
// the sweep so a rebuilt repository offers every artifact to its next push.
var bookkeepingTables = []string{"unclustered", "unsent"}

// rebuildTableList reports every table Rebuild reconstructs, for Report.
var rebuildTableList = append(manifest.RederivedTables(), bookkeepingTables...)

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

	// The rebuilt mlink is what elision is read from. The store cone is not a
	// derived table and survives the rebuild, so elided phantoms stay elided.
	if err := countAbsences(r, report); err != nil {
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

// rederive re-derives the crosslink tables (manifest.Rederive), applies the
// horizon seed a structural-horizon clone holds outside the artifact store,
// as draft-fossil-horizon-00 §5.2.3 requires of every derivation, and refills
// the sync-bookkeeping tables the sweep does not own. manifest.Rederive runs
// CrosslinkRebuild, not Crosslink, so the whole rebuild runs in tx: a check-in
// whose files or parent are absent is linked with phantoms, and a delta
// manifest whose baseline is absent is refused and filed in orphan, exactly as
// Crosslink and `fossil rebuild` do.
func rederive(tx *db.Tx) error {
	if _, err := manifest.Rederive(tx); err != nil {
		return err
	}
	// A held seed is re-verified against the clone's key (§5.2.4: every seed
	// it holds), and discarded if it fails; the next sync falls back.
	verify, err := seed.VerifierFor(tx)
	if err != nil {
		return err
	}
	if _, err := seed.Apply(tx, verify); err != nil {
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
	// A pushed artifact not yet judged, or judged a seed, is never queued to
	// be announced or sent (seedcheck).
	withheld, err := seedcheck.Withheld(tx)
	if err != nil {
		return fmt.Errorf("rebuildBookkeeping: %w", err)
	}
	for _, table := range bookkeepingTables {
		_, err := tx.Exec("INSERT INTO " + table + "(rid) SELECT rid FROM blob WHERE size >= 0")
		if err != nil {
			return fmt.Errorf("rebuildBookkeeping %s: %w", table, err)
		}
		for rid := range withheld {
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE rid=?", rid); err != nil {
				return fmt.Errorf("rebuildBookkeeping %s: %w", table, err)
			}
		}
	}
	return nil
}
