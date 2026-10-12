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

		// Derive the tag the way crosslinking the control artifact would. A
		// superseded tag leaves tagxref alone, but the control artifact is
		// still stored and queued for sync.
		if err := ApplyTagWithTx(tx, ApplyOpts{
			TargetRID: opts.TargetRID,
			SrcRID:    controlRid,
			TagName:   opts.TagName,
			TagType:   opts.TagType,
			Value:     opts.Value,
			MTime:     libfossil.TimeToJulian(opts.Time),
		}); err != nil {
			return err
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
		return ApplyTagWithTx(tx, opts)
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
		tagid, opts.TagType, opts.SrcRID, opts.TargetRID, nullableValue(opts.Value),
		opts.MTime, opts.TargetRID,
	); err != nil {
		return fmt.Errorf("tagxref insert: %w", err)
	}

	if err := applyEventOverride(q, opts); err != nil {
		return err
	}

	if err := applyDateOverride(q, opts); err != nil {
		return err
	}

	if err := propagateAfterInsert(q, tagid, opts.TagType, opts.TargetRID, opts.MTime, opts.Value, opts.TagName); err != nil {
		return fmt.Errorf("propagate: %w", err)
	}

	return nil
}

// eventTagColumns maps the tags that override a check-in's derived event row
// to the column each one sets, as tag_insert's switch does (src/tag.c).
var eventTagColumns = map[string]string{
	"bgcolor": "bgcolor",
	"user":    "euser",
	"comment": "ecomment",
}

// applyEventOverride sets event.bgcolor, euser or ecomment from a bgcolor,
// user or comment tag that has just won its tagxref row. Like tag_insert it
// does this for every tag type, writing NULL for a cancel, so a singleton or
// cancelled bgcolor takes effect too. A check-in crosslinked later reads the
// same rows back; see crosslinkCheckinTables.
func applyEventOverride(q db.Querier, opts ApplyOpts) error {
	col, ok := eventTagColumns[opts.TagName]
	if !ok {
		return nil
	}
	var value any
	if opts.TagType != TagCancel {
		value = opts.Value
	}
	// col comes from the fixed map above, never from input.
	stmt := "UPDATE event SET " + col + "=? WHERE objid=?"
	if _, err := q.Exec(stmt, value, opts.TargetRID); err != nil {
		return fmt.Errorf("%s update: %w", opts.TagName, err)
	}
	return nil
}

// applyDateOverride moves a check-in's event.mtime to the value of a date tag
// (`fossil amend --date`) that has just won its tagxref row, keeping the
// manifest's own time in omtime. It mirrors tag_insert (src/tag.c), which runs
// this for every tag type: a cancel's NULL value leaves mtime NULL there too.
// A check-in crosslinked later reads the same row back; see
// crosslinkCheckinTables.
func applyDateOverride(q db.Querier, opts ApplyOpts) error {
	if opts.TagName != "date" {
		return nil
	}
	var value any
	if opts.TagType != TagCancel {
		value = nullableValue(opts.Value)
	}
	if _, err := q.Exec(
		`UPDATE event SET mtime=julianday(?), omtime=coalesce(omtime, mtime) WHERE objid=?`,
		value, opts.TargetRID,
	); err != nil {
		return fmt.Errorf("date update: %w", err)
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

// propagateAfterInsert runs the propagation half of canonical's tag_insert,
// which downgrades a singleton to a cancel before propagating and then
// propagates unconditionally (src/tag.c:239):
//
//	if( tagtype==1 ) tagtype = 0;
//	tag_propagate(rid, tagid, tagtype, rid, zValue, mtime);
//
// A singleton (+) tag therefore does not merely fail to propagate its own
// value -- it actively blocks a same-named propagating tag at this artifact,
// deleting the copies descendants had inherited. Confirmed against fossil
// 2.28: applying `tag add --raw zz A v2` over an inherited `zz` removes the
// child's row.
func propagateAfterInsert(q db.Querier, tagid int64, tagType int, rid libfossil.FslID, mtime float64, value, tagName string) error {
	if tagType == TagSingleton {
		tagType = TagCancel
	}
	return propagate(q, tagid, tagType, rid, mtime, value, tagName, rid)
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

// nullableValue is the tagxref.value for a tag value: NULL for a tag that has
// none, as fossil stores it (#258). A T card cannot carry an empty value, so
// "" always means absent; storing an empty string would read as a tag with
// an empty value.
func nullableValue(v string) any {
	if v == "" {
		return nil
	}
	return v
}
