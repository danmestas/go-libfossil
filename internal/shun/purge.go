package shun

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// PurgeResult summarizes what Purge deleted.
type PurgeResult struct {
	BlobsDeleted   int
	DeltasExpanded int
	PrivateCleaned int
}

// Purge physically removes all shunned blobs from the repo.
// Follows Fossil's shun_artifacts() algorithm from shun.c.
// Requires *db.DB (not Querier) because it runs in a transaction.
func Purge(d *db.DB) (PurgeResult, error) {
	if d == nil {
		panic("shun.Purge: d must not be nil")
	}
	var result PurgeResult

	err := d.WithTx(func(tx *db.Tx) error {
		// Step 1: Find rids to delete.
		_, err := tx.Exec(`CREATE TEMP TABLE toshun(rid INTEGER PRIMARY KEY)`)
		if err != nil {
			return fmt.Errorf("create toshun: %w", err)
		}

		_, err = tx.Exec(`INSERT INTO toshun SELECT rid FROM blob WHERE uuid IN (SELECT uuid FROM shun)`)
		if err != nil {
			return fmt.Errorf("populate toshun: %w", err)
		}

		// Step 2: Undelta dependents — expand blobs whose delta source is being shunned.
		rows, err := tx.Query(`SELECT rid FROM delta WHERE srcid IN (SELECT rid FROM toshun)`)
		if err != nil {
			return fmt.Errorf("query dependents: %w", err)
		}
		var dependents []libfossil.FslID
		for rows.Next() {
			var rid libfossil.FslID
			if err := rows.Scan(&rid); err != nil {
				rows.Close()
				return fmt.Errorf("scan dependent: %w", err)
			}
			dependents = append(dependents, rid)
		}
		rows.Close()

		for _, rid := range dependents {
			expanded, err := content.Expand(tx, rid)
			if err != nil {
				return fmt.Errorf("expand rid %d: %w", rid, err)
			}
			compressed, err := blob.Compress(expanded)
			if err != nil {
				return fmt.Errorf("compress rid %d: %w", rid, err)
			}
			_, err = tx.Exec("UPDATE blob SET content=?, size=? WHERE rid=?",
				compressed, len(expanded), rid)
			if err != nil {
				return fmt.Errorf("update blob rid %d: %w", rid, err)
			}
			_, err = tx.Exec("DELETE FROM delta WHERE rid=?", rid)
			if err != nil {
				return fmt.Errorf("delete delta rid %d: %w", rid, err)
			}
			result.DeltasExpanded++
		}

		// Step 3: Delete shunned blobs.
		_, err = tx.Exec("DELETE FROM delta WHERE rid IN (SELECT rid FROM toshun)")
		if err != nil {
			return fmt.Errorf("delete shunned deltas: %w", err)
		}

		res, err := tx.Exec("DELETE FROM blob WHERE rid IN (SELECT rid FROM toshun)")
		if err != nil {
			return fmt.Errorf("delete shunned blobs: %w", err)
		}
		n, _ := res.RowsAffected()
		result.BlobsDeleted = int(n)

		_, err = tx.Exec("DROP TABLE toshun")
		if err != nil {
			return fmt.Errorf("drop toshun: %w", err)
		}

		// Step 4: Orphan cleanup.
		res, err = tx.Exec("DELETE FROM private WHERE NOT EXISTS (SELECT 1 FROM blob WHERE rid=private.rid)")
		if err != nil {
			return fmt.Errorf("cleanup private: %w", err)
		}
		n, _ = res.RowsAffected()
		result.PrivateCleaned = int(n)

		return nil
	})

	return result, err
}
