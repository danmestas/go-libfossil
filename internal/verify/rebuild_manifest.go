package verify

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// rebuildManifests walks all non-phantom blobs, parses checkin manifests,
// and inserts event/plink/mlink/filename rows.
func rebuildManifests(r *repo.Repo, tx *db.Tx, report *Report, cache *content.Cache) error {
	if r == nil {
		panic("rebuildManifests: nil *repo.Repo")
	}
	if tx == nil {
		panic("rebuildManifests: nil *db.Tx")
	}
	if report == nil {
		panic("rebuildManifests: nil *Report")
	}

	entries, err := collectBlobEntries(tx)
	if err != nil {
		return err
	}

	for _, e := range entries {
		data, err := cache.Expand(tx, e.rid)
		if err != nil {
			report.BlobsSkipped++
			continue // not expandable — corrupt, raw data blob, or phantom
		}
		d, err := deck.Parse(data)
		if err != nil {
			continue // not a manifest — normal for file blobs
		}
		if d.Type != deck.Checkin {
			continue
		}
		if err := rebuildCheckin(tx, e.rid, d, report, cache); err != nil {
			return fmt.Errorf("rebuildManifests rid=%d: %w", e.rid, err)
		}
	}
	return nil
}

// blobEntry holds a blob's rid and uuid for rebuild iteration.
type blobEntry struct {
	rid  libfossil.FslID
	uuid string
}

// collectBlobEntries reads all non-phantom blob rid/uuid pairs.
func collectBlobEntries(q db.Querier) ([]blobEntry, error) {
	rows, err := q.Query("SELECT rid, uuid FROM blob WHERE size >= 0")
	if err != nil {
		return nil, fmt.Errorf("collectBlobEntries: %w", err)
	}
	defer rows.Close()

	var entries []blobEntry
	for rows.Next() {
		var e blobEntry
		if err := rows.Scan(&e.rid, &e.uuid); err != nil {
			return nil, fmt.Errorf("collectBlobEntries scan: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// rebuildCheckin inserts event, plink, and mlink rows for one checkin manifest.
func rebuildCheckin(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, report *Report, cache *content.Cache) error {
	if tx == nil {
		panic("rebuildCheckin: nil *db.Tx")
	}
	if d == nil {
		panic("rebuildCheckin: nil *deck.Deck")
	}

	mtime := libfossil.TimeToJulian(d.D)

	// Insert event row
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO event(type, mtime, objid, user, comment) VALUES('ci', ?, ?, ?, ?)",
		mtime, rid, d.U, d.C,
	); err != nil {
		return fmt.Errorf("event: %w", err)
	}

	// Insert plink rows for parent(s)
	if err := rebuildPlinks(tx, rid, d, mtime, report); err != nil {
		return err
	}

	// Derive mlink/filename rows through the shared canonical implementation.
	if err := manifest.DeriveCheckinMlinks(tx, cache, rid, d); err != nil {
		return err
	}

	return nil
}

// rebuildPlinks inserts plink rows for each parent in the manifest.
func rebuildPlinks(tx *db.Tx, rid libfossil.FslID, d *deck.Deck, mtime float64, report *Report) error {
	for i, parentUUID := range d.P {
		parentRID, ok := blob.Exists(tx, parentUUID)
		if !ok {
			report.MissingRefs++
			report.addIssue(Issue{
				Kind:    IssueMissingReference,
				RID:     rid,
				UUID:    parentUUID,
				Table:   "plink",
				Message: fmt.Sprintf("rid=%d parent %s not found", rid, parentUUID),
			})
			continue
		}
		isPrim := 0
		if i == 0 {
			isPrim = 1
		}
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO plink(pid, cid, isprim, mtime) VALUES(?, ?, ?, ?)",
			parentRID, rid, isPrim, mtime,
		); err != nil {
			return fmt.Errorf("plink: %w", err)
		}
	}
	return nil
}
