package checkout

import (
	"fmt"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// recordableMTime is the vfile.mtime a scan or a write stores for a file
// whose mtime is mtime, in fossil's unit: whole seconds since 1970.
//
// This is the one deliberate difference from fossil. Fossil's
// vfile_check_signature (src/vfile.c) stores the mtime as it is and later
// skips hashing while size and mtime match, so it misses an edit that keeps
// the size within the second it recorded. Here an mtime not older than now's
// second is stored as 0, which makes the next scan, fossil's or ours, hash
// the file once; a stored 0 is never trusted (scanSingleEntry).
func recordableMTime(mtime time.Time, now int64) int64 {
	secs := mtime.Unix()
	if secs >= now {
		return 0
	}
	return secs
}

// writtenMTime returns the vfile.mtime to record for the file at fullPath,
// statting it right after it was written, as fossil's vfile_to_disk does.
// now is read before the stat, so a write after the stat cannot slip past
// the guard with an mtime equal to the one recorded.
func (c *Checkout) writtenMTime(fullPath string) (int64, error) {
	now := c.env.Clock.Now().Unix()
	info, err := c.env.Storage.Stat(fullPath)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", fullPath, err)
	}
	return recordableMTime(info.ModTime(), now), nil
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
