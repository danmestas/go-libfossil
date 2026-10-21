package checkout

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// RequireCommitContent checks parent structure without scanning working files,
// changing the queue, or fetching content. Recovery belongs to the caller.
func (c *Checkout) RequireCommitContent(ctx context.Context) error {
	if c == nil || ctx == nil {
		panic("checkout.RequireCommitContent: checkout and context must be set")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, _, err := c.Version()
	if err != nil {
		return err
	}
	parents, err := c.mergeParents(parent)
	if err != nil {
		return err
	}
	return manifest.RequireParentContent(ctx, c.repo.DB(), append([]libfossil.FslID{parent}, parents...))
}

// RecoverySnapshot identifies checkout metadata and the in-memory selection
// across an unlocked recovery exchange. Working files are deliberately not
// scanned: a retry must read their fresh bytes rather than cache a file list.
func (c *Checkout) RecoverySnapshot() (string, error) {
	if c == nil {
		panic("checkout.RecoverySnapshot: nil checkout")
	}
	tx, err := c.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	digest := sha256.New()
	encoder := json.NewEncoder(digest)
	queries := []string{
		"SELECT name,value FROM vvar WHERE name IN ('checkout','checkout-hash','sparse-cone') ORDER BY name",
		"SELECT * FROM vmerge ORDER BY id,merge",
		"SELECT * FROM vfile ORDER BY id",
	}
	var pending bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='sparse_pending')").Scan(&pending); err != nil {
		return "", err
	}
	if pending {
		queries = append(queries, "SELECT * FROM sparse_pending ORDER BY vid,pathname")
	}
	for _, query := range queries {
		if err := encodeRecoveryRows(tx, encoder, query); err != nil {
			return "", err
		}
	}
	if err := encoder.Encode(c.checkinQueue); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func encodeRecoveryRows(tx *sql.Tx, encoder *json.Encoder, query string) error {
	if err := encoder.Encode(query); err != nil {
		return err
	}
	rows, err := tx.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	if err := encoder.Encode(columns); err != nil {
		return err
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		if err := encoder.Encode(values); err != nil {
			return err
		}
	}
	return rows.Err()
}
