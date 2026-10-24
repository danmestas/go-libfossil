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
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
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
	// SrcRID is the artifact whose T card introduced this tag: the control
	// artifact, or the check-in carrying an inline card. tagxref keeps 0 for
	// inherited rows only, so a date tag needs it positive to set a time.
	SrcRID  libfossil.FslID
	TagName string
	TagType int // TagCancel, TagSingleton, or TagPropagating.
	Value   string
	MTime   float64 // Julian day.
	// Leaves is the batch's leaf-recheck schedule. A branch tag, and the
	// propagation walk it starts, schedule the check-ins whose branch row
	// they write (draft-fossil-repo-state-00 Section 7.3); the caller applies
	// the schedule before its batch ends. ApplyTagWithTx requires it for a
	// branch tag; ApplyTag supplies its own when it is nil.
	Leaves *leaf.Pending
}

// AddTag creates a control artifact that adds or cancels a tag on a target checkin.
// It stores the artifact as a blob, ensures the tag name exists in the tag table,
// inserts/replaces a row in the tagxref table, and writes the artifact's 'g'
// event row, as crosslinking it would.
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
	// The D card holds the time as fossil writes it: to the millisecond, a
	// caller's time rounded as fossil rounds a --date-override. Finer digits
	// are legal (§4.6.4) but zit cannot read them.
	if opts.Time.IsZero() {
		opts.Time = libfossil.CardTimeNow(time.Now())
	} else {
		opts.Time = libfossil.CardTime(opts.Time)
	}

	var controlRid libfossil.FslID

	// The control artifact is one receipt, credited to the tagging user.
	uid, err := r.LocalUID(opts.User)
	if err != nil {
		return 0, fmt.Errorf("tag.AddTag: %w", err)
	}
	err = r.WithReceipt(&db.Receipt{UID: uid}).WithTx(func(tx *db.Tx) error {
		d, err := controlDeck(tx, opts)
		if err != nil {
			return err
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
		// still stored and queued for sync. The leaf rechecks a branch tag
		// schedules are applied before this transaction commits, as fossil
		// applies them at the end of its own (src/db.c, leaf_do_pending_checks).
		leaves := leaf.NewPending()
		if err := ApplyTagWithTx(tx, ApplyOpts{
			TargetRID: opts.TargetRID,
			SrcRID:    controlRid,
			TagName:   opts.TagName,
			TagType:   opts.TagType,
			Value:     opts.Value,
			MTime:     libfossil.TimeToJulian(opts.Time),
			Leaves:    leaves,
		}); err != nil {
			return err
		}
		if err := leaves.Apply(tx); err != nil {
			return err
		}
		if err := InsertControlEvent(tx, controlRid, d); err != nil {
			return err
		}

		// Mark control artifact as unsent so sync pushes it (unclustered is handled by blob.Store).
		if _, err := tx.Exec("INSERT OR IGNORE INTO unsent(rid) VALUES(?)", controlRid); err != nil {
			return fmt.Errorf("tag.AddTag: unsent: %w", err)
		}
		// The control artifact may fill a pushed delta's basis; a seed it
		// makes readable is judged in this transaction (seedcheck).
		_, err = seedcheck.JudgeFilled(tx, []libfossil.FslID{controlRid})
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("tag.AddTag: %w", err)
	}
	return controlRid, nil
}

// controlDeck builds the control artifact AddTag stores: one T card naming
// the target by its hash.
func controlDeck(q db.Querier, opts TagOpts) (*deck.Deck, error) {
	var targetUUID string
	const lookup = "SELECT uuid FROM blob WHERE rid=?"
	if err := q.QueryRow(lookup, opts.TargetRID).Scan(&targetUUID); err != nil {
		return nil, fmt.Errorf("target uuid lookup: %w", err)
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
		return nil, fmt.Errorf("invalid tag type: %d", opts.TagType)
	}
	return &deck.Deck{
		Type: deck.Control,
		D:    opts.Time,
		T: []deck.TagCard{{
			Type: deckTagType, Name: opts.TagName, UUID: targetUUID, Value: opts.Value,
		}},
		U: deck.User(opts.User),
	}, nil
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
		if opts.Leaves != nil {
			return ApplyTagWithTx(tx, opts)
		}
		// The transaction is the batch: its rechecks are applied before it
		// commits.
		opts.Leaves = leaf.NewPending()
		if err := ApplyTagWithTx(tx, opts); err != nil {
			return err
		}
		return opts.Leaves.Apply(tx)
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
	if tagid == leaf.BranchTagID && opts.Leaves == nil {
		panic("tag.ApplyTagWithTx: a branch tag needs opts.Leaves")
	}

	skip, err := superseded(q, tagid, opts.TargetRID, opts.MTime)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	reach, err := replacesPropagatingBranch(q, tagid, opts)
	if err != nil {
		return err
	}
	if _, err := q.Exec(
		`INSERT OR REPLACE INTO tagxref(tagid, tagtype, srcid, origid, value, mtime, rid)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		tagid, opts.TagType, opts.SrcRID, opts.TargetRID, nullableValue(opts.Value),
		opts.MTime, opts.TargetRID,
	); err != nil {
		return fmt.Errorf("tagxref insert: %w", err)
	}
	// tag_insert schedules the check-in and its parents once its branch row
	// is written (src/tag.c:195), whatever the tag type.
	if tagid == leaf.BranchTagID {
		if err := opts.Leaves.Schedule(q, opts.TargetRID); err != nil {
			return err
		}
	}

	if err := applyEventOverride(q, opts); err != nil {
		return err
	}

	if err := applyDateOverride(q, opts); err != nil {
		return err
	}

	if err := propagateAfterInsert(q, tagid, opts, reach); err != nil {
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
		// Empty maps to NULL so this agrees with the tagxref read-back
		// crosslink does later.
		value = nullableValue(opts.Value)
	}
	// col comes from the fixed map above, never from input.
	stmt := "UPDATE event SET " + col + "=? WHERE objid=?"
	if _, err := q.Exec(stmt, value, opts.TargetRID); err != nil {
		return fmt.Errorf("%s update: %w", opts.TagName, err)
	}
	return nil
}

// applyDateOverride re-derives a check-in's event time from its date tag
// (`fossil amend --date`) once that tag has won its tagxref row.
func applyDateOverride(q db.Querier, opts ApplyOpts) error {
	if opts.TagName != "date" {
		return nil
	}
	if err := RefreshEventTime(q, opts.TargetRID); err != nil {
		return fmt.Errorf("date update: %w", err)
	}
	return nil
}

// RefreshEventTime sets rid's event.mtime from its date tag, following
// draft-fossil-repo-state-00 Section 4.2, step 6: the value of a date tag
// applied directly to rid (srcid > 0), when it parses (by SQLite
// julianday, which already bounds the upper end) to a positive time;
// otherwise the D-card time kept in event.omtime. An inherited row
// (srcid 0, from a propagating tag on an ancestor) and a cancel set nothing.
//
// Both arrival orders call this, tag application after it wins its row and
// crosslink after it inserts the event row, so the result depends only on
// tagxref, never on which came first. That is where -00 departs from fossil
// 2.28: its tag_insert writes julianday(value) as is, so a cancel or an
// unparsable value leaves no time, 0 stores a time of 0, and its
// manifest_crosslink reads an inherited row too. omtime keeps the D-card
// time (RS-4.2-15); coalesce fills it for an event row that predates it.
// A missing event row (a phantom target) is a no-op.
func RefreshEventTime(q db.Querier, rid libfossil.FslID) error {
	if q == nil {
		panic("tag.RefreshEventTime: q must not be nil")
	}
	if rid <= 0 {
		panic("tag.RefreshEventTime: rid must be positive")
	}
	// In an UPDATE every right-hand side reads the row as it was, so the
	// fallback is the stored omtime, or, for a row without one, the old mtime.
	_, err := q.Exec(
		`UPDATE event SET
		   mtime=coalesce((SELECT jd FROM (SELECT julianday(value) AS jd FROM tagxref
		       WHERE rid=?1 AND srcid>0 AND tagtype>0
		         AND tagid=(SELECT tagid FROM tag WHERE tagname='date'))
		     WHERE jd>0 AND jd<5373484.5), omtime, mtime),
		   omtime=coalesce(omtime, mtime)
		 WHERE objid=?1`,
		rid,
	)
	return err
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
func propagateAfterInsert(
	q db.Querier, tagid int64, opts ApplyOpts, reachAbsent bool,
) error {
	tagType := opts.TagType
	if tagType == TagSingleton {
		tagType = TagCancel
	}
	pr := propagation{
		tagid: tagid, tagType: tagType, origID: opts.TargetRID, mtime: opts.MTime,
		value: opts.Value, tagName: opts.TagName, leaves: opts.Leaves,
		reachAbsent: reachAbsent && tagType == TagCancel,
	}
	return pr.propagate(q, opts.TargetRID)
}

// replacesPropagatingBranch is whether this write replaces a propagating
// branch row with a cancel or a singleton. The row it replaces was handed down
// the primary children when it landed, in fossil, and the walk deletes those
// copies and rechecks each (src/tag.c:100-102). A crosslink batch defers
// propagation to its repair gate, so the copies may not exist yet; the walk
// then reaches the children that hold none (propagation.reachAbsent), which are
// the check-ins fossil would have rechecked (Section 7.3, RS-7.3-3). Only a
// branch tag matters: no other tag schedules a recheck.
func replacesPropagatingBranch(q db.Querier, tagid int64, opts ApplyOpts) (bool, error) {
	if tagid != leaf.BranchTagID || opts.TagType == TagPropagating {
		return false, nil
	}
	var old int
	err := q.QueryRow("SELECT tagtype FROM tagxref WHERE tagid=? AND rid=?",
		tagid, opts.TargetRID).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("tagxref branch row: %w", err)
	}
	return old == TagPropagating, nil
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
