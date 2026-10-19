package checkout

import (
	"fmt"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// writtenMTime returns the vfile.mtime to record for the file at fullPath,
// just written (vfile.WrittenMTime).
func (c *Checkout) writtenMTime(fullPath string) (int64, error) {
	return vfile.WrittenMTime(c.env, fullPath)
}

// recordMTime stores mtime, from writtenMTime, on the row of version vid
// named pathname when the row is based on artifact rid, the content that was
// written. A row based on another artifact keeps its mtime, so a row only
// ever vouches for the content it names.
func (c *Checkout) recordMTime(
	q sqlExecer, vid libfossil.FslID, pathname string, rid, mtime int64,
) error {
	if rid <= 0 {
		panic("checkout.recordMTime: rid must be positive")
	}
	if _, err := q.Exec(
		"UPDATE vfile SET mtime = ? WHERE vid = ? AND pathname = ? AND mrid = ?",
		mtime, int64(vid), pathname, rid,
	); err != nil {
		return fmt.Errorf("record mtime for %s: %w", pathname, err)
	}
	return nil
}

// inheritMTimes gives each row of version vid with no recorded mtime the one
// a row of another version recorded for the same file, where that row vouches
// for the same artifact: unchanged (chnged=0), not removed, and with the same
// mrid. It is fossil's update moving an unchanged file's mtime to the target
// version's row, and its commit keeping vfile rows in place.
func (c *Checkout) inheritMTimes(q sqlExecer, vid libfossil.FslID) error {
	if vid <= 0 {
		panic("checkout.inheritMTimes: vid must be positive")
	}
	_, err := q.Exec(`
		UPDATE vfile SET mtime = old.mtime
		  FROM vfile AS old
		 WHERE vfile.vid = ? AND coalesce(vfile.mtime, 0) = 0
		   AND old.vid != vfile.vid AND old.pathname = vfile.pathname
		   AND old.chnged = 0 AND NOT old.deleted AND old.mrid = vfile.mrid
		   AND old.mtime != 0`, int64(vid))
	if err != nil {
		return fmt.Errorf("inherit mtimes: %w", err)
	}
	return nil
}

// rowMTimes returns the recorded mtimes of version vid's rows named in names.
func (c *Checkout) rowMTimes(vid libfossil.FslID, names []string) (map[string]int64, error) {
	out := make(map[string]int64, len(names))
	for _, name := range names {
		var mtime int64
		err := c.db.QueryRow(
			"SELECT CAST(coalesce(mtime, 0) AS INTEGER) FROM vfile WHERE vid = ? AND pathname = ?",
			int64(vid), name,
		).Scan(&mtime)
		if err != nil {
			return nil, fmt.Errorf("read mtime of %s: %w", name, err)
		}
		if mtime != 0 {
			out[name] = mtime
		}
	}
	return out, nil
}

// setRowMTimes stores mtimes on version vid's rows, by pathname.
func (c *Checkout) setRowMTimes(vid libfossil.FslID, mtimes map[string]int64) error {
	for name, mtime := range mtimes {
		if _, err := c.db.Exec(
			"UPDATE vfile SET mtime = ? WHERE vid = ? AND pathname = ?",
			mtime, int64(vid), name,
		); err != nil {
			return fmt.Errorf("set mtime of %s: %w", name, err)
		}
	}
	return nil
}
