package checkout

import (
	"fmt"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// fileCheckinTimesQuery is fossil's mtime_of_manifest_file (src/descendants.c)
// for every file at once: for each artifact, the time of the newest check-in
// that recorded it among vid and vid's primary ancestors (compute_ancestors
// in direct mode), in seconds since 1970. The seconds are computed and cast
// as fossil computes and reads them, so a time is truncated, not rounded.
const fileCheckinTimesQuery = `
	WITH RECURSIVE ancestor(rid, mtime) AS (
	  SELECT ?, mtime FROM event WHERE objid=?
	  UNION
	  SELECT plink.pid, event.mtime
	    FROM ancestor, plink, event
	   WHERE plink.cid=ancestor.rid
	     AND event.objid=plink.pid
	     AND plink.isPrim
	   ORDER BY mtime DESC LIMIT 100000000
	)
	SELECT mlink.fid, CAST((max(event.mtime)-2440587.5)*86400 AS INTEGER)
	  FROM mlink, event
	 WHERE mlink.mid=event.objid
	   AND mlink.mid IN (SELECT rid FROM ancestor)
	 GROUP BY mlink.fid`

// fileCheckinTimes returns, by file artifact, the seconds fossil's --setmtime
// gives a file of version vid (fileCheckinTimesQuery).
func (c *Checkout) fileCheckinTimes(vid libfossil.FslID) (map[int64]int64, error) {
	rows, err := c.repo.DB().Query(fileCheckinTimesQuery, int64(vid), int64(vid))
	if err != nil {
		return nil, fmt.Errorf("file check-in times: %w", err)
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var fid, secs int64
		if err := rows.Scan(&fid, &secs); err != nil {
			return nil, fmt.Errorf("file check-in times: %w", err)
		}
		out[fid] = secs
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("file check-in times: %w", err)
	}
	return out, nil
}

// setFileMTimes is fossil's --setmtime, vfile_check_signature with
// CKSIG_SETMTIME (src/vfile.c): after the files of vid are brought up to date
// by hash, every one still unchanged (chnged 0, 2 or 4) and present on disk
// takes the time fileCheckinTimes gives its artifact. As in fossil it is
// best-effort: a file whose time cannot be set keeps the one it has.
func (c *Checkout) setFileMTimes(vid libfossil.FslID) error {
	if vid <= 0 {
		panic("checkout.setFileMTimes: vid must be positive")
	}
	if err := c.refreshChanged(vid); err != nil {
		return err
	}
	times, err := c.fileCheckinTimes(vid)
	if err != nil {
		return err
	}
	rows, err := c.db.Query(`SELECT pathname, rid FROM vfile
		WHERE vid=? AND NOT deleted AND rid>0 AND chnged IN (0, 2, 4)`, int64(vid))
	if err != nil {
		return fmt.Errorf("set mtimes: %w", err)
	}
	defer rows.Close()
	set := map[string]int64{}
	for rows.Next() {
		var name string
		var rid int64
		if err := rows.Scan(&name, &rid); err != nil {
			return fmt.Errorf("set mtimes: %w", err)
		}
		// An artifact no check-in in the ancestry recorded is absent from
		// times, and gets 0: fossil's max() over no rows is NULL, which it
		// reads as 0, setting the file to 1970.
		secs := times[rid]
		fullPath, err := c.safePath(name)
		if err != nil {
			continue
		}
		info, err := c.env.Storage.Stat(fullPath)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		when := time.Unix(secs, 0)
		if c.env.Storage.Chtimes(fullPath, when, when) == nil {
			set[name] = recordableMTime(when, c.env.Clock.Now().Unix())
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("set mtimes: %w", err)
	}
	// As fossil's scan, the time given a file is the one vfile records.
	return c.setRowMTimes(vid, set)
}
