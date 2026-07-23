package sync

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// applyXDeleteLocally applies an xdelete to the local repo. Calls DeleteXRowByPKHash first;
// if the row doesn't exist, inserts a tombstone from pkData. Returns nil on success.
func applyXDeleteLocally(d *db.DB, table string, def repo.TableDef, pkHash string, mtime int64, pkData []byte) error {
	deleted, err := repo.DeleteXRowByPKHash(d, table, def, pkHash, mtime)
	if err != nil {
		return fmt.Errorf("applyXDeleteLocally: delete %s/%s: %w", table, pkHash, err)
	}
	if deleted {
		return nil
	}

	// Row didn't exist or had newer mtime. Check which case.
	existingRow, _, err := repo.LookupXRow(d, table, def, pkHash)
	if err != nil {
		return fmt.Errorf("applyXDeleteLocally: lookup %s/%s: %w", table, pkHash, err)
	}
	if existingRow != nil {
		return nil // Row exists with newer mtime — no-op.
	}

	// Row doesn't exist. Insert tombstone from PKData.
	if len(pkData) == 0 {
		return nil // No PKData — can't create tombstone.
	}
	dec := json.NewDecoder(bytes.NewReader(pkData))
	dec.UseNumber()
	var pkValues map[string]any
	if err := dec.Decode(&pkValues); err != nil {
		return fmt.Errorf("applyXDeleteLocally: decode PKData %s/%s: %w", table, pkHash, err)
	}
	coerceJSONNumbers(pkValues, def)

	// Verify PKData matches declared PKHash.
	var pkColDefs []repo.ColumnDef
	for _, col := range def.Columns {
		if col.PK {
			pkColDefs = append(pkColDefs, col)
		}
	}
	computedHash := repo.PKHash(pkColDefs, pkValues)
	if computedHash != pkHash {
		return fmt.Errorf("applyXDeleteLocally: PKData hash mismatch %s/%s: computed %s", table, pkHash, computedHash)
	}

	if err := repo.UpsertXRow(d, table, pkValues, mtime); err != nil {
		return fmt.Errorf("applyXDeleteLocally: insert tombstone %s/%s: %w", table, pkHash, err)
	}
	return nil
}
