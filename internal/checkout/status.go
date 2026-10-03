package checkout

import (
	"errors"
	"fmt"
	"os"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

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
	rows, err := vfile.Load(c.db, int64(rid))
	if err != nil {
		return false, fmt.Errorf("checkout.HasChanges: %w", err)
	}
	for _, r := range rows {
		if vfile.Classify(r, false) != ChangeNone {
			return true, nil
		}
	}
	return false, nil
}

// VisitChanges calls fn for each changed file of version vid, classified by
// vfile.Classify. With scan=true it first runs ScanChanges(ScanHash) and
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
	rows, err := vfile.Load(c.db, int64(vid))
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
		change := vfile.Classify(r, missing)
		if change == ChangeNone {
			continue
		}
		entry := ChangeEntry{
			Name:     r.Pathname,
			Change:   change,
			VFileID:  libfossil.FslID(r.ID),
			IsExec:   r.IsExe != 0,
			IsLink:   r.IsLink != 0,
			OrigName: r.RenamedFrom(),
		}
		if err := fn(entry); err != nil {
			return fmt.Errorf("checkout.VisitChanges: visitor for %s: %w", r.Pathname, err)
		}
	}
	return nil
}

// isMissing reports whether a row's file is absent from disk, the way
// fossil's changes command decides MISSING: anything that is not a regular
// file counts. A removed row is never missing; it is meant to be gone.
func (c *Checkout) isMissing(r vfile.Row) (bool, error) {
	if r.Pathname == "" {
		panic("checkout.isMissing: empty pathname")
	}
	if c.env == nil {
		panic("checkout.isMissing: nil env")
	}
	if r.Deleted > 0 {
		return false, nil
	}
	fullPath, err := c.safePath(r.Pathname)
	if err != nil {
		return false, fmt.Errorf("path traversal in %s: %w", r.Pathname, err)
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
