package content

import (
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

const (
	// ClusterThreshold is the minimum number of unclustered, non-phantom blobs
	// before cluster generation triggers.
	ClusterThreshold = 100

	// ClusterMaxSize is the maximum number of M-cards per cluster artifact.
	ClusterMaxSize = 800
)

// GenerateClusters creates cluster artifacts for unclustered blobs that are
// not phantoms, shunned, or private. Returns the number of clusters created.
func GenerateClusters(q db.Querier) (int, error) {
	if q == nil {
		panic("content.GenerateClusters: q must not be nil")
	}

	uuids, err := clusterableUUIDs(q)
	if err != nil {
		return 0, err
	}
	if len(uuids) < ClusterThreshold {
		return 0, nil
	}

	clusterRIDs, err := buildClusters(q, uuids)
	if err != nil {
		return len(clusterRIDs), err
	}

	if err := cleanupUnclustered(q, clusterRIDs); err != nil {
		return len(clusterRIDs), err
	}
	return len(clusterRIDs), nil
}

// clusterableUUIDs returns sorted UUIDs of unclustered blobs that are not
// phantoms, shunned, or private.
func clusterableUUIDs(q db.Querier) ([]string, error) {
	if q == nil {
		panic("content.clusterableUUIDs: q must not be nil")
	}

	rows, err := q.Query(`
		SELECT b.uuid FROM unclustered u
		JOIN blob b ON b.rid = u.rid
		WHERE NOT EXISTS (SELECT 1 FROM phantom WHERE rid = u.rid)
		  AND NOT EXISTS (SELECT 1 FROM shun WHERE uuid = b.uuid)
		  AND NOT EXISTS (SELECT 1 FROM private WHERE rid = u.rid)
		ORDER BY b.uuid
	`)
	if err != nil {
		return nil, fmt.Errorf("content.clusterableUUIDs query: %w", err)
	}
	defer rows.Close()

	var uuids []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, fmt.Errorf("content.clusterableUUIDs scan: %w", err)
		}
		uuids = append(uuids, uuid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("content.clusterableUUIDs rows: %w", err)
	}
	return uuids, nil
}

// buildClusters batches UUIDs into cluster artifacts, stores them, and applies
// the cluster tag. Returns the RIDs of created cluster blobs.
func buildClusters(q db.Querier, uuids []string) ([]libfossil.FslID, error) {
	if q == nil {
		panic("content.buildClusters: q must not be nil")
	}

	var clusterRIDs []libfossil.FslID
	for len(uuids) > 0 {
		batchSize := ClusterMaxSize
		if batchSize > len(uuids) {
			batchSize = len(uuids)
		}
		// Only split if more than ClusterThreshold remain after this batch.
		remaining := len(uuids) - batchSize
		if remaining > 0 && remaining < ClusterThreshold {
			batchSize = len(uuids) // take all
		}

		batch := uuids[:batchSize]
		uuids = uuids[batchSize:]

		d := &deck.Deck{Type: deck.Cluster, M: batch}
		data, err := d.Marshal()
		if err != nil {
			return clusterRIDs, fmt.Errorf("content.buildClusters marshal: %w", err)
		}

		rid, _, err := blob.Store(q, data)
		if err != nil {
			return clusterRIDs, fmt.Errorf("content.buildClusters store: %w", err)
		}

		// Apply cluster singleton tag (tagid=7, tagtype=1).
		// Inlined to avoid import cycle (content -> manifest -> content).
		if _, err := q.Exec(
			"INSERT OR REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid) VALUES(7, 1, ?, ?, NULL, 0, ?)",
			rid, rid, rid,
		); err != nil {
			return clusterRIDs, fmt.Errorf("content.buildClusters tag: %w", err)
		}

		clusterRIDs = append(clusterRIDs, rid)
	}
	return clusterRIDs, nil
}

// cleanupUnclustered removes non-phantom, non-shunned, non-private entries
// from the unclustered table, preserving only the newly created cluster blobs.
func cleanupUnclustered(q db.Querier, clusterRIDs []libfossil.FslID) error {
	if q == nil {
		panic("content.cleanupUnclustered: q must not be nil")
	}
	if len(clusterRIDs) == 0 {
		return nil
	}

	placeholders := make([]string, len(clusterRIDs))
	args := make([]any, len(clusterRIDs))
	for i, rid := range clusterRIDs {
		placeholders[i] = "?"
		args[i] = rid
	}

	query := fmt.Sprintf(`
		DELETE FROM unclustered
		WHERE rid NOT IN (%s)
		  AND NOT EXISTS (SELECT 1 FROM phantom WHERE rid = unclustered.rid)
		  AND NOT EXISTS (SELECT 1 FROM shun
		      WHERE uuid = (SELECT uuid FROM blob WHERE rid = unclustered.rid))
		  AND NOT EXISTS (SELECT 1 FROM private WHERE rid = unclustered.rid)`,
		strings.Join(placeholders, ","),
	)
	if _, err := q.Exec(query, args...); err != nil {
		return fmt.Errorf("content.cleanupUnclustered: %w", err)
	}
	return nil
}
