// Package vfile reads a checkout's vfile table and says what each row means.
//
// The vfile table is fossil's record of a checkout: one row per tracked file.
// Its columns encode state in ways that are easy to misread: rid=0 marks a
// pending add, origname equals pathname on a row that was never renamed,
// mrid differs from rid while a merge is pending, and chnged carries fossil's
// merge codes (2 through 9) as well as a plain edit (1). Every caller that
// needs to know what a row means asks this package instead of decoding the
// columns itself, so fossil's encoding is interpreted in exactly one place.
package vfile

import (
	"database/sql"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
)

// Change is how a tracked file differs from the checked-out version.
type Change int

const (
	ChangeNone Change = iota
	ChangeAdded
	ChangeRemoved
	ChangeMissing
	ChangeRenamed
	ChangeModified
)

// Row is one vfile row.
type Row struct {
	ID       int64
	Pathname string
	Origname sql.NullString
	Chnged   int64
	Deleted  int64
	RID      int64
	IsExe    int64
	IsLink   int64
}

// IsAdded reports a pending add: a file with no committed version (rid=0).
// Fossil records an add with chnged left 0, so rid alone decides.
func (r Row) IsAdded() bool {
	return r.RID == 0
}

// IsRemoved reports a pending removal.
func (r Row) IsRemoved() bool {
	return r.Deleted > 0
}

// RenamedFrom returns the row's prior pathname when it records a rename, or
// "". Fossil stores origname==pathname on rows that were never renamed.
func (r Row) RenamedFrom() string {
	if !r.Origname.Valid {
		return ""
	}
	if r.Origname.String == r.Pathname {
		return ""
	}
	return r.Origname.String
}

// ContentChanged reports whether the file's content is not its committed
// version: a pending add, an edit, or a merge outcome.
func (r Row) ContentChanged() bool {
	if r.IsAdded() {
		return true
	}
	return r.Chnged > 0
}

// PendingMerge reports a merge outcome fossil has recorded but not yet
// committed (chnged 2 through 9).
func (r Row) PendingMerge() bool {
	return r.Chnged > 1
}

// Classify maps a row to its change type, in fossil's priority order (the
// changes command): removed > missing > added > renamed > modified. missing
// reports that the file is absent from disk; callers that have not looked
// pass false. One difference from fossil: a renamed file that is also edited
// is Renamed, not Modified, so callers that see only the change type keep
// the rename.
func Classify(r Row, missing bool) Change {
	if r.Pathname == "" {
		panic("vfile.Classify: empty pathname")
	}
	if r.Chnged < 0 {
		panic("vfile.Classify: negative chnged for " + r.Pathname)
	}
	if r.IsRemoved() {
		return ChangeRemoved
	}
	if missing {
		return ChangeMissing
	}
	if r.IsAdded() {
		return ChangeAdded
	}
	if r.RenamedFrom() != "" {
		return ChangeRenamed
	}
	if r.Chnged > 0 {
		return ChangeModified
	}
	return ChangeNone
}

// Load reads every row of version vid, ordered by pathname as fossil's
// changes command lists them. Rows are collected up front so no cursor is
// held open while callers read files or update rows.
func Load(q db.Querier, vid int64) ([]Row, error) {
	if q == nil {
		panic("vfile.Load: nil querier")
	}
	if vid < 0 {
		panic("vfile.Load: negative vid")
	}

	rows, err := q.Query(`
		SELECT id, pathname, origname, CAST(chnged AS INTEGER), CAST(deleted AS INTEGER),
		       rid, CAST(isexe AS INTEGER), CAST(islink AS INTEGER)
		FROM vfile WHERE vid = ? ORDER BY pathname`, vid)
	if err != nil {
		return nil, fmt.Errorf("vfile.Load: query: %w", err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(
			&r.ID, &r.Pathname, &r.Origname, &r.Chnged, &r.Deleted,
			&r.RID, &r.IsExe, &r.IsLink,
		); err != nil {
			return nil, fmt.Errorf("vfile.Load: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vfile.Load: iterate: %w", err)
	}
	return out, nil
}
