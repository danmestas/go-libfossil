package checkout

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// vfileRow is one checkout row as the change classifier reads it. Every
// question about what a row means goes through classifyChange, so fossil's
// encoding (rid=0 for an add, origname==pathname on unrenamed rows, chnged
// codes) is interpreted in one place.
type vfileRow struct {
	id       int64
	pathname string
	origname sql.NullString
	chnged   int64
	deleted  int64
	rid      int64
	isexe    int64
	islink   int64
}

// renamedFrom returns the row's prior pathname when it records a rename, or
// "". Fossil stores origname==pathname on rows that were never renamed.
func (r vfileRow) renamedFrom() string {
	if !r.origname.Valid {
		return ""
	}
	if r.origname.String == r.pathname {
		return ""
	}
	return r.origname.String
}

// classifyChange maps a row to its change type, in fossil's priority order
// (the changes command): removed > missing > added > renamed > modified.
// missing reports that the file is absent from disk; callers that have not
// looked pass false. One difference from fossil: a renamed file that is also
// edited reports as renamed, not edited, so the rename is not lost to callers
// that see only the change type.
func classifyChange(r vfileRow, missing bool) FileChange {
	if r.pathname == "" {
		panic("checkout.classifyChange: empty pathname")
	}
	if r.chnged < 0 {
		panic("checkout.classifyChange: negative chnged for " + r.pathname)
	}
	if r.deleted > 0 {
		return ChangeRemoved
	}
	if missing {
		return ChangeMissing
	}
	if r.rid == 0 {
		return ChangeAdded
	}
	if r.renamedFrom() != "" {
		return ChangeRenamed
	}
	if r.chnged > 0 {
		return ChangeModified
	}
	return ChangeNone
}

// loadVFileRows reads every row of version vid, ordered by pathname as
// fossil's changes command lists them. They are collected up front so no
// cursor is held open while callers read files or update rows.
func (c *Checkout) loadVFileRows(vid libfossil.FslID) ([]vfileRow, error) {
	if c == nil {
		panic("checkout.loadVFileRows: nil *Checkout")
	}
	if vid < 0 {
		panic("checkout.loadVFileRows: negative vid")
	}

	rows, err := c.db.Query(`
		SELECT id, pathname, origname, CAST(chnged AS INTEGER), CAST(deleted AS INTEGER),
		       rid, CAST(isexe AS INTEGER), CAST(islink AS INTEGER)
		FROM vfile WHERE vid = ? ORDER BY pathname`, int64(vid))
	if err != nil {
		return nil, fmt.Errorf("query vfile: %w", err)
	}
	defer rows.Close()

	var out []vfileRow
	for rows.Next() {
		var r vfileRow
		if err := rows.Scan(
			&r.id, &r.pathname, &r.origname, &r.chnged, &r.deleted,
			&r.rid, &r.isexe, &r.islink,
		); err != nil {
			return nil, fmt.Errorf("scan vfile: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vfile: %w", err)
	}
	return out, nil
}

// HasChanges reports whether any file of the current version is added,
// removed, renamed or modified. It reads only the checkout database: an edit
// on disk counts once a scan has recorded it, but adds, removals and renames
// are always seen. Missing files are not, because that needs the disk.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) HasChanges() (bool, error) {
	if c == nil {
		panic("checkout.HasChanges: nil *Checkout")
	}

	rid, _, err := c.Version()
	if err != nil {
		return false, fmt.Errorf("checkout.HasChanges: %w", err)
	}
	if rid < 0 {
		panic("checkout.HasChanges: negative checkout version")
	}
	rows, err := c.loadVFileRows(rid)
	if err != nil {
		return false, fmt.Errorf("checkout.HasChanges: %w", err)
	}
	for _, r := range rows {
		if classifyChange(r, false) != ChangeNone {
			return true, nil
		}
	}
	return false, nil
}

// VisitChanges calls fn for each changed file of version vid, classified by
// classifyChange. With scan=true it first runs ScanChanges(ScanHash) and
// checks each file on disk, so edits and missing files are reported; with
// scan=false it reports what the checkout database records.
//
// If fn returns a non-nil error, iteration stops and that error is returned.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) VisitChanges(vid libfossil.FslID, scan bool, fn ChangeVisitor) error {
	if c == nil {
		panic("checkout.VisitChanges: nil *Checkout")
	}
	if fn == nil {
		panic("checkout.VisitChanges: nil visitor")
	}

	if scan {
		if err := c.ScanChanges(ScanHash); err != nil {
			return fmt.Errorf("checkout.VisitChanges: %w", err)
		}
	}
	rows, err := c.loadVFileRows(vid)
	if err != nil {
		return fmt.Errorf("checkout.VisitChanges: %w", err)
	}
	for _, r := range rows {
		missing := false
		if scan {
			if missing, err = c.isMissing(r); err != nil {
				return fmt.Errorf("checkout.VisitChanges: %w", err)
			}
		}
		change := classifyChange(r, missing)
		if change == ChangeNone {
			continue
		}
		entry := ChangeEntry{
			Name:     r.pathname,
			Change:   change,
			VFileID:  libfossil.FslID(r.id),
			IsExec:   r.isexe != 0,
			IsLink:   r.islink != 0,
			OrigName: r.renamedFrom(),
		}
		if err := fn(entry); err != nil {
			return fmt.Errorf("checkout.VisitChanges: visitor for %s: %w", r.pathname, err)
		}
	}
	return nil
}

// isMissing reports whether a row's file is absent from disk, the way
// fossil's changes command decides MISSING: anything that is not a regular
// file counts. A removed row is never missing; it is meant to be gone.
func (c *Checkout) isMissing(r vfileRow) (bool, error) {
	if r.pathname == "" {
		panic("checkout.isMissing: empty pathname")
	}
	if c.env == nil {
		panic("checkout.isMissing: nil env")
	}
	if r.deleted > 0 {
		return false, nil
	}
	fullPath, err := c.safePath(r.pathname)
	if err != nil {
		return false, fmt.Errorf("path traversal in %s: %w", r.pathname, err)
	}
	info, err := c.env.Storage.Stat(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("stat %s: %w", fullPath, err)
	}
	return !info.Mode().IsRegular(), nil
}
