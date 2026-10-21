package checkout

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

const sparsePendingSchema = `CREATE TABLE IF NOT EXISTS sparse_pending(
 vid INTEGER NOT NULL,
 pathname TEXT NOT NULL,
 rid INTEGER NOT NULL,
 mrid INTEGER NOT NULL,
 chnged INTEGER NOT NULL DEFAULT 0,
 deleted INTEGER NOT NULL DEFAULT 0,
 isexe INTEGER NOT NULL DEFAULT 0,
 islink INTEGER NOT NULL DEFAULT 0,
 origname TEXT,
 mhash TEXT,
 pivot INTEGER NOT NULL DEFAULT 0,
 remote INTEGER NOT NULL DEFAULT 0,
 unresolved INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(vid,pathname)
)`

type sparseOutcome struct {
	vid                       int64
	row                       vfile.Row
	mhash                     sql.NullString
	pivot, remote, unresolved int64
}

func (c *Checkout) snapshotPending() ([]sparseOutcome, error) {
	if c.cone == nil {
		return nil, nil
	}
	rows, err := c.db.Query("SELECT vid,pathname,rid,mrid,chnged,deleted,isexe,islink,origname,mhash,pivot,remote,unresolved FROM sparse_pending ORDER BY vid,pathname")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sparseOutcome
	for rows.Next() {
		var s sparseOutcome
		if err := rows.Scan(&s.vid, &s.row.Pathname, &s.row.RID, &s.row.MRID, &s.row.Chnged, &s.row.Deleted, &s.row.IsExe, &s.row.IsLink, &s.row.Origname, &s.mhash, &s.pivot, &s.remote, &s.unresolved); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s sparseOutcome) save(q sqlExecer) error {
	_, err := q.Exec(`INSERT OR REPLACE INTO sparse_pending(vid,pathname,rid,mrid,chnged,deleted,isexe,islink,origname,mhash,pivot,remote,unresolved) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.vid, s.row.Pathname, s.row.RID, s.row.MRID, s.row.Chnged, s.row.Deleted, s.row.IsExe, s.row.IsLink, s.row.Origname, s.mhash, s.pivot, s.remote, s.unresolved)
	return err
}

// migrateSparseRows removes legacy uncovered vfile records only after native
// checkout discovery has been disabled. Pending outcomes remain authoritative.
func (c *Checkout) migrateSparseRows() (err error) {
	if c.cone == nil {
		return nil
	}
	vid, _, err := c.Version()
	if err != nil {
		return err
	}
	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil {
		return err
	}
	outside := false
	for _, row := range rows {
		outside = outside || !c.covers(row.Pathname)
	}
	if !outside {
		return c.moveMarker(sparseDBName)
	}
	files, err := manifest.ListFiles(c.repo, vid)
	if err != nil {
		return err
	}
	baseline := make(map[string]manifest.FileEntry, len(files))
	for _, f := range files {
		baseline[f.Name] = f
	}
	oldName := filepath.Base(c.dbPath)
	if err := c.moveMarker(sparseDBName); err != nil {
		return err
	}
	tx, err := c.db.Begin()
	if err != nil {
		return errors.Join(err, c.moveMarker(oldName))
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback(), c.moveMarker(oldName))
		}
	}()
	for _, row := range rows {
		if c.covers(row.Pathname) {
			continue
		}
		f, exists := baseline[row.Pathname]
		changed := !exists || row.ContentChanged() || row.IsRemoved() || row.PendingMerge() || row.RenamedFrom() != "" || (row.IsExe > 0) != (f.Perm == "x") || (row.IsLink > 0) != (f.Perm == "l")
		if changed {
			var pending bool
			if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM sparse_pending WHERE vid=? AND pathname=?)", int64(vid), row.Pathname).Scan(&pending); err != nil {
				return err
			}
			if !pending {
				outcome := sparseOutcome{vid: int64(vid), row: row}
				if err := outcome.save(tx); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec("DELETE FROM vfile WHERE id=?", row.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ensureSparsePending(q sqlExecer) error {
	_, err := q.Exec(sparsePendingSchema)
	return err
}

// effectiveRows derives uncovered baseline records from the repository. Only
// pending outcomes are checkout-local; an unchanged uncovered file has no row.
func (c *Checkout) effectiveRows(vid libfossil.FslID) ([]vfile.Row, error) {
	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil || c.cone == nil || vid == 0 {
		return rows, err
	}
	files, err := manifest.ListFiles(c.repo, vid)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]vfile.Row, len(files)+len(rows))
	for _, row := range rows {
		byName[row.Pathname] = row
	}
	for _, f := range files {
		if c.covers(f.Name) {
			continue
		}
		var rid int64
		if err := c.repo.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", f.UUID).Scan(&rid); err != nil {
			return nil, fmt.Errorf("uncovered baseline %s: %w", f.Name, err)
		}
		row := vfile.Row{Pathname: f.Name, RID: rid, MRID: rid}
		if f.Perm == "x" {
			row.IsExe = 1
		}
		if f.Perm == "l" {
			row.IsLink = 1
		}
		byName[f.Name] = row
	}
	pending, err := c.pendingRows(vid)
	if err != nil {
		return nil, err
	}
	for _, row := range pending {
		byName[row.Pathname] = row
	}
	rows = rows[:0]
	for _, row := range byName {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Pathname < rows[j].Pathname })
	for i := range rows {
		if rows[i].ID <= 0 {
			rows[i].ID = -int64(i + 1)
		}
	}
	return rows, nil
}

func (c *Checkout) pendingRows(vid libfossil.FslID) ([]vfile.Row, error) {
	if c.cone == nil {
		return nil, nil
	}
	rows, err := c.db.Query(`SELECT pathname,origname,chnged,deleted,rid,mrid,isexe,islink FROM sparse_pending WHERE vid=? ORDER BY pathname`, int64(vid))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []vfile.Row
	for rows.Next() {
		var row vfile.Row
		if err := rows.Scan(&row.Pathname, &row.Origname, &row.Chnged, &row.Deleted, &row.RID, &row.MRID, &row.IsExe, &row.IsLink); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (c *Checkout) applyRecordOnlyMerge(tx *sql.Tx, vid libfossil.FslID, s mergeStep) error {
	row := s.row
	row.Pathname = s.file.Name
	unresolved := 0
	switch s.file.Action {
	case MergeUpdated:
		row.MRID, row.Chnged = int64(s.merged.rid), 2
		row.IsExe, row.IsLink = boolInt(s.merged.exe), boolInt(s.merged.link)
	case MergeAdded:
		row.RID, row.MRID, row.Chnged = int64(s.merged.rid), int64(s.merged.rid), 3
		row.IsExe, row.IsLink = boolInt(s.merged.exe), boolInt(s.merged.link)
	case MergeDeleted:
		row.Deleted = 1
	case MergeMode:
		row.IsExe = boolInt(s.exe)
	case MergeConflict:
		unresolved = 1
		row.Chnged = 2
	case MergeKept, MergeForked:
		return nil
	default:
		return fmt.Errorf("unsupported record-only merge action %d", s.file.Action)
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO sparse_pending(vid,pathname,rid,mrid,chnged,deleted,isexe,islink,origname,mhash,pivot,remote,unresolved) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		int64(vid), row.Pathname, row.RID, row.MRID, row.Chnged, row.Deleted, row.IsExe, row.IsLink, row.Origname, s.merged.uuid, int64(s.pivotRID), int64(s.merged.rid), unresolved)
	return err
}

func boolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func (c *Checkout) planEnteringConflict(vid libfossil.FslID, row vfile.Row, outcome sparseOutcome) (mergeStep, error) {
	step := mergeStep{file: MergeFile{Name: row.Pathname}, row: row, exe: row.IsExe > 0, merged: mergeSide{rid: libfossil.FslID(outcome.remote), uuid: outcome.mhash.String}}
	if outcome.remote <= 0 || row.IsRemoved() || row.IsLink > 0 {
		step.file.Action = MergeKept
		return step, nil
	}
	choose, err := c.mergeStrategyChooser(vid, "")
	if err != nil {
		return mergeStep{}, err
	}
	err = c.planThreeWay(&step, mergeSide{rid: libfossil.FslID(outcome.pivot)}, choose)
	return step, err
}

func insertManagedRow(tx *sql.Tx, vid libfossil.FslID, row vfile.Row) (int64, error) {
	_, err := tx.Exec(`INSERT OR IGNORE INTO vfile(vid,pathname,rid,mrid,chnged,deleted,isexe,islink,origname) VALUES(?,?,?,?,?,?,?,?,?)`, int64(vid), row.Pathname, row.RID, row.MRID, row.Chnged, row.Deleted, row.IsExe, row.IsLink, row.Origname)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow("SELECT id FROM vfile WHERE vid=? AND pathname=?", int64(vid), row.Pathname).Scan(&id)
	return id, err
}

func (c *Checkout) unresolvedSparse() ([]string, error) {
	if c.cone == nil {
		return nil, nil
	}
	rows, err := c.db.Query("SELECT pathname FROM sparse_pending WHERE unresolved<>0 ORDER BY pathname")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
