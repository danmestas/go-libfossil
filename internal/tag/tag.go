package tag

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

const (
	TagCancel      = 0
	TagSingleton   = 1
	TagPropagating = 2
)

// TagOpts describes a tag operation on a target artifact.
type TagOpts struct {
	TargetRID libfossil.FslID
	TagName   string
	TagType   int // TagCancel, TagSingleton, or TagPropagating
	Value     string
	User      string
	Time      time.Time
}

// ApplyOpts describes a tag application from an existing control artifact.
type ApplyOpts struct {
	TargetRID libfossil.FslID // Artifact to tag.
	SrcRID    libfossil.FslID // Control artifact that introduced this tag (0 for inline T-cards).
	TagName   string
	TagType   int // TagCancel, TagSingleton, or TagPropagating.
	Value     string
	MTime     float64 // Julian day.
}

// AddTag creates a control artifact that adds or cancels a tag on a target checkin.
// It stores the artifact as a blob, ensures the tag name exists in the tag table,
// and inserts/replaces a row in the tagxref table.
func AddTag(r *repo.Repo, opts TagOpts) (libfossil.FslID, error) {
	if r == nil {
		panic("tag.AddTag: r must not be nil")
	}
	if opts.TagName == "" {
		panic("tag.AddTag: opts.TagName must not be empty")
	}
	if opts.TargetRID <= 0 {
		panic("tag.AddTag: opts.TargetRID must be positive")
	}
	if opts.User == "" {
		panic("tag.AddTag: opts.User must not be empty")
	}
	if opts.Time.IsZero() {
		opts.Time = time.Now().UTC()
	}

	var controlRid libfossil.FslID

	err := r.WithTx(func(tx *db.Tx) error {
		// Look up target UUID
		var targetUUID string
		if err := tx.QueryRow("SELECT uuid FROM blob WHERE rid=?", opts.TargetRID).Scan(&targetUUID); err != nil {
			return fmt.Errorf("target uuid lookup: %w", err)
		}

		// Map our integer tag type to deck.TagType byte
		var deckTagType deck.TagType
		switch opts.TagType {
		case TagCancel:
			deckTagType = deck.TagCancel
		case TagSingleton:
			deckTagType = deck.TagSingleton
		case TagPropagating:
			deckTagType = deck.TagPropagating
		default:
			return fmt.Errorf("invalid tag type: %d", opts.TagType)
		}

		// Build control artifact deck
		d := &deck.Deck{
			Type: deck.Control,
			D:    opts.Time,
			T: []deck.TagCard{
				{
					Type:  deckTagType,
					Name:  opts.TagName,
					UUID:  targetUUID,
					Value: opts.Value,
				},
			},
			U: deck.User(opts.User),
		}

		// Marshal and store as blob
		manifestBytes, err := d.Marshal()
		if err != nil {
			return fmt.Errorf("marshal control artifact: %w", err)
		}
		rid, _, err := blob.Store(tx, manifestBytes)
		if err != nil {
			return fmt.Errorf("store control artifact: %w", err)
		}
		controlRid = rid

		// Ensure tag name exists in tag table
		tagid, err := ensureTag(tx, opts.TagName)
		if err != nil {
			return fmt.Errorf("ensure tag %q: %w", opts.TagName, err)
		}

		// Insert or replace tagxref row
		mtime := libfossil.TimeToJulian(opts.Time)
		skip, err := superseded(tx, tagid, opts.TargetRID, mtime)
		if err != nil {
			return err
		}
		if skip {
			// The control artifact is still stored and queued for sync --
			// only the derived tagxref state is left alone.
			if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", controlRid); err != nil {
				return fmt.Errorf("tag.AddTag: unsent: %w", err)
			}
			return nil
		}
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`,
			tagid, opts.TagType, controlRid, opts.TargetRID, opts.Value, mtime, opts.TargetRID,
		); err != nil {
			return fmt.Errorf("tagxref insert: %w", err)
		}

		// Propagate to descendants (matches Fossil's tag_insert → tag_propagate).
		if opts.TagType == TagPropagating || opts.TagType == TagCancel {
			if err := propagate(tx, tagid, opts.TagType, opts.TargetRID, mtime, opts.Value, opts.TagName, opts.TargetRID); err != nil {
				return fmt.Errorf("tag propagate: %w", err)
			}
		}

		// Mark control artifact as unsent so sync pushes it (unclustered is handled by blob.Store).
		if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", controlRid); err != nil {
			return fmt.Errorf("tag.AddTag: unsent: %w", err)
		}

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("tag.AddTag: %w", err)
	}
	return controlRid, nil
}

// ApplyTag inserts a tagxref row and propagates without creating a control artifact.
// Used by Crosslink to process existing control artifacts.
func ApplyTag(r *repo.Repo, opts ApplyOpts) error {
	if r == nil {
		panic("tag.ApplyTag: r must not be nil")
	}
	if opts.TagName == "" {
		panic("tag.ApplyTag: opts.TagName must not be empty")
	}
	if opts.TargetRID <= 0 {
		panic("tag.ApplyTag: opts.TargetRID must be positive")
	}

	return r.WithTx(func(tx *db.Tx) error {
		tagid, err := ensureTag(tx, opts.TagName)
		if err != nil {
			return fmt.Errorf("ensure tag %q: %w", opts.TagName, err)
		}

		skip, err := superseded(tx, tagid, opts.TargetRID, opts.MTime)
		if err != nil {
			return err
		}
		if skip {
			return nil
		}

		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`,
			tagid, opts.TagType, opts.SrcRID, opts.TargetRID, opts.Value, opts.MTime, opts.TargetRID,
		); err != nil {
			return fmt.Errorf("tagxref insert: %w", err)
		}

		// Special: bgcolor updates event table.
		if opts.TagName == "bgcolor" && opts.TagType == TagPropagating {
			if _, err := tx.Exec("UPDATE event SET bgcolor=? WHERE objid=?", opts.Value, opts.TargetRID); err != nil {
				return fmt.Errorf("bgcolor update: %w", err)
			}
		}

		if opts.TagType == TagPropagating || opts.TagType == TagCancel {
			if err := propagate(tx, tagid, opts.TagType, opts.TargetRID, opts.MTime, opts.Value, opts.TagName, opts.TargetRID); err != nil {
				return fmt.Errorf("propagate: %w", err)
			}
		}

		return nil
	})
}

// ApplyTagWithTx inserts a tagxref row and propagates using an existing transaction.
// This avoids the nested-transaction problem when called from within Rebuild's
// single wrapping transaction. Identical logic to ApplyTag but accepts a db.Querier.
func ApplyTagWithTx(q db.Querier, opts ApplyOpts) error {
	if q == nil {
		panic("tag.ApplyTagWithTx: q must not be nil")
	}
	if opts.TagName == "" {
		panic("tag.ApplyTagWithTx: opts.TagName must not be empty")
	}
	if opts.TargetRID <= 0 {
		panic("tag.ApplyTagWithTx: opts.TargetRID must be positive")
	}

	tagid, err := ensureTag(q, opts.TagName)
	if err != nil {
		return fmt.Errorf("ensure tag %q: %w", opts.TagName, err)
	}

	skip, err := superseded(q, tagid, opts.TargetRID, opts.MTime)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	if _, err := q.Exec(
		`INSERT OR REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		tagid, opts.TagType, opts.SrcRID, opts.TargetRID, opts.Value, opts.MTime, opts.TargetRID,
	); err != nil {
		return fmt.Errorf("tagxref insert: %w", err)
	}

	// Special: bgcolor updates event table.
	if opts.TagName == "bgcolor" && opts.TagType == TagPropagating {
		if _, err := q.Exec("UPDATE event SET bgcolor=? WHERE objid=?", opts.Value, opts.TargetRID); err != nil {
			return fmt.Errorf("bgcolor update: %w", err)
		}
	}

	if opts.TagType == TagPropagating || opts.TagType == TagCancel {
		if err := propagate(q, tagid, opts.TagType, opts.TargetRID, opts.MTime, opts.Value, opts.TagName, opts.TargetRID); err != nil {
			return fmt.Errorf("propagate: %w", err)
		}
	}

	return nil
}

// superseded reports whether tagxref already holds a row for (tagid, rid)
// whose mtime is at least as new as the application being attempted.
//
// Canonical fossil opens tag_insert with exactly this query and returns
// without touching tagxref -- and, just as importantly, without calling
// tag_propagate -- when it matches (src/tag.c:173-186):
//
//	SELECT 1 FROM tagxref WHERE tagid=%d AND rid=%d AND mtime>=:mtime
//	...
//	if( rc==SQLITE_ROW ){
//	  /* Another entry that is more recent already exists.  Do nothing */
//	  return tagid;
//	}
//
// This is what makes the resulting tagxref state independent of the order
// artifacts are applied in. A check-in's inline T-cards and a control artifact
// that later retagged the same check-in reach us in whichever order the sweep
// visits them; the newer one has to win either way. Dropping the propagation
// matters as much as dropping the write: replaying a stale branch value from
// a check-in that has since been retagged floods every descendant with it
// (issue #198).
//
// The comparison is >=, not >, so equal mtimes leave the standing row alone,
// matching fossil.
func superseded(q db.Querier, tagid int64, rid libfossil.FslID, mtime float64) (bool, error) {
	var one int
	err := q.QueryRow(
		"SELECT 1 FROM tagxref WHERE tagid=? AND rid=? AND mtime>=?", tagid, rid, mtime,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("tagxref mtime check: %w", err)
	}
	return true, nil
}

// ensureTag returns the tagid for the given tag name, creating it if it doesn't exist.
func ensureTag(q db.Querier, name string) (int64, error) {
	if q == nil {
		panic("tag.ensureTag: q must not be nil")
	}
	if name == "" {
		panic("tag.ensureTag: name must not be empty")
	}
	var tagid int64
	err := q.QueryRow("SELECT tagid FROM tag WHERE tagname=?", name).Scan(&tagid)
	if err == nil {
		return tagid, nil
	}
	result, err := q.Exec("INSERT INTO tag(tagname) VALUES(?)", name)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}
