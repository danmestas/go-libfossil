package checkout

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// The sparse worktree of draft-fossil-sparse-cone-00 §4.
//
// The cone is checkout-local state (§3.1), kept in the vvar table as the
// lowercase hex of its canonical serialization. Sparse metadata uses a
// separate checkout marker so native Fossil cannot interpret uncovered
// records as missing or deleted working files.

// coneVVar is the vvar holding the cone; absent means dense.
const coneVVar = "sparse-cone"

// ErrOutsideCone is wrapped by the refusal of every operation that would act
// on a path the checkout's cone does not cover. The message names the path,
// the cone and the member that excluded it, as §4.1 requires: a diagnostic
// that reports a missing file, or alleges damage, does not conform.
var ErrOutsideCone = errors.New("outside the checkout's sparse cone")

// maxConeNames bounds how many paths a cone refusal names.
const maxConeNames = 10

// readCone loads the stored cone, nil when the checkout is dense. A stored
// cone that does not parse fails: it is authoritative state, and reading it
// as dense would track and commit what the operator excluded.
func readCone(ckdb *sql.DB) (*cone.Cone, error) {
	value, err := getVVar(ckdb, coneVVar)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil
	}
	data, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("stored sparse cone: %w: %v", cone.ErrInvalidMember, err)
	}
	c, err := cone.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("stored sparse cone: %w", err)
	}
	return c, nil
}

// writeCone stores c, or removes the stored cone when c is nil.
func writeCone(ckdb sqlExecer, c *cone.Cone) error {
	if c == nil {
		if _, err := ckdb.Exec("DELETE FROM vvar WHERE name = ?", coneVVar); err != nil {
			return fmt.Errorf("clear sparse cone: %w", err)
		}
		return nil
	}
	_, err := ckdb.Exec("REPLACE INTO vvar(name,value) VALUES(?,?)", coneVVar, hex.EncodeToString(c.Serialize()))
	return err
}

// Cone returns the checkout's cone, or nil for a dense checkout.
func (c *Checkout) Cone() *cone.Cone {
	if c == nil {
		panic("checkout.Cone: nil *Checkout")
	}
	return c.cone
}

// covers reports whether the checkout's cone covers name; a dense checkout
// covers everything.
func (c *Checkout) covers(name string) bool {
	return c.cone == nil || c.cone.Covers(name)
}

// fileFilter is the cone as the manifest enumeration seam takes it: nil for
// a dense checkout, since a nil *cone.Cone in the interface would not be.
func (c *Checkout) fileFilter() manifest.FileFilter {
	if c.cone == nil {
		return nil
	}
	return c.cone
}

// managedRows is the managed view of version vid's record: the rows the cone
// covers. Every operation that enumerates the files a checkout manages reads
// them here, so the cone is applied in one place for the record (§4.1), as
// manifest.ListFilesFiltered applies it for check-ins.
func (c *Checkout) managedRows(vid libfossil.FslID) ([]vfile.Row, error) {
	return c.coneRows(vid, true)
}

// coneRows is the single range predicate for checkout records. Managed rows
// describe disk; the complementary rows carry the uncovered baseline and
// pending record-only operations.
func (c *Checkout) coneRows(vid libfossil.FslID, managed bool) ([]vfile.Row, error) {
	if !managed {
		return c.pendingRows(vid)
	}
	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil {
		return nil, err
	}
	kept := rows[:0]
	for _, r := range rows {
		if c.covers(r.Pathname) == managed {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// commitBaseline carries the baseline's uncovered entries, including pending
// record-only merge changes. The managed file set is synthesized separately
// from disk; uncovered pending content is addressed by mrid, never a path.
func (c *Checkout) commitBaseline(vid libfossil.FslID) ([]manifest.FileEntry, error) {
	files, err := manifest.ListFiles(c.repo, vid)
	if err != nil || c.cone == nil {
		return files, err
	}
	rows, err := c.coneRows(vid, false)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]manifest.FileEntry, len(files))
	for _, f := range files {
		byName[f.Name] = f
	}
	for _, r := range rows {
		if r.IsRemoved() {
			delete(byName, r.Pathname)
			continue
		}
		f := byName[r.Pathname]
		if r.ContentChanged() || r.PendingMerge() || f.Name == "" {
			uuid, err := blobUUID(c.repo.DB(), libfossil.FslID(r.MRID))
			if err != nil {
				return nil, err
			}
			f.Name, f.UUID = r.Pathname, uuid
		}
		f.Perm = ""
		if r.IsExe != 0 {
			f.Perm = "x"
		}
		if r.IsLink != 0 {
			f.Perm = "l"
		}
		byName[r.Pathname] = f
	}
	files = files[:0]
	for _, f := range byName {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// outsideCone is the refusal of operation op on paths the cone does not
// cover, naming the paths, the member that excluded each, and the cone.
func (c *Checkout) outsideCone(op string, paths []string) error {
	if c.cone == nil || len(paths) == 0 {
		panic("checkout.outsideCone: nothing is outside the cone")
	}
	named := paths
	if len(named) > maxConeNames {
		named = named[:maxConeNames]
	}
	quoted := make([]string, len(named))
	for i, p := range named {
		quoted[i] = strconv.Quote(p)
		if d := c.cone.Decide(p); d.Excluded() {
			quoted[i] += " (excluded by !" + d.Member + ")"
		}
	}
	more := ""
	if len(paths) > len(named) {
		more = fmt.Sprintf(" and %d more", len(paths)-len(named))
	}
	return fmt.Errorf("checkout.%s: %s%s: %w (cone: %s); widen the cone to cover it, or return to dense",
		op, strings.Join(quoted, ", "), more, ErrOutsideCone, c.cone)
}

// CheckSparsePaths validates paths for operations that hold a checkout SQL
// handle rather than a Checkout, before they read or write working files.
func CheckSparsePaths(ckdb *sql.DB, op string, paths []string) error {
	if ckdb == nil || op == "" {
		panic("checkout.CheckSparsePaths: database and operation must be set")
	}
	sparse, err := readCone(ckdb)
	if err != nil {
		return err
	}
	c := Checkout{cone: sparse}
	return c.refuseUncovered(op, paths...)
}

// refuseUncovered refuses op when any of paths lies outside the cone.
func (c *Checkout) refuseUncovered(op string, paths ...string) error {
	var outside []string
	for _, p := range paths {
		if !c.covers(p) {
			outside = append(outside, p)
		}
	}
	if len(outside) == 0 {
		return nil
	}
	return c.outsideCone(op, outside)
}

// refuseUnreachable refuses op on the names among paths whose subtree the
// cone cannot reach: a name selecting no managed file because the cone
// excludes everything under it.
func (c *Checkout) refuseUnreachable(op string, paths []string) error {
	if c.cone == nil {
		return nil
	}
	var outside []string
	for _, p := range paths {
		if !c.cone.IntersectsSubtree(path.Clean(p)) {
			outside = append(outside, p)
		}
	}
	if len(outside) == 0 {
		return nil
	}
	return c.outsideCone(op, outside)
}

// ConeChange reports what SetCone did to the working tree. Both lists are
// sorted.
type ConeChange struct {
	Materialized   []string // newly covered files written to disk
	Dematerialized []string // newly uncovered files removed from disk
}

// conePlan is a cone change worked out before anything moves.
type conePlan struct {
	entering []vfile.Row // rows the new cone covers and the old one did not
	leaving  []vfile.Row // rows the old cone covers and the new one does not
	mtimes   map[string]int64
	merges   map[string]mergeStep
	pending  []sparseOutcome
}

// SetCone replaces the checkout's cone; nil returns it to dense. Replacing
// wholesale and amending member by member are the same operation (§3.5):
// build the new cone with cone.New, or with the current cone's With and
// Without. Newly covered files of the checked-out version are written;
// newly uncovered ones are removed from disk, which is all narrowing does
// to a checkout -- the repository keeps every artifact.
//
// It refuses, changing nothing, when a file leaving the cone has local
// changes (an edit, add, removal, rename, merge, or a missing file), and
// when a file entering it would overwrite an untracked file of different
// content.
func (c *Checkout) SetCone(next *cone.Cone) (ConeChange, error) {
	if c == nil {
		panic("checkout.SetCone: nil *Checkout")
	}
	vid, _, err := c.Version()
	if err != nil {
		return ConeChange{}, fmt.Errorf("checkout.SetCone: %w", err)
	}
	var plan conePlan
	if vid > 0 {
		if plan, err = c.planConeChange(vid, next); err != nil {
			return ConeChange{}, fmt.Errorf("checkout.SetCone: %w", err)
		}
	}
	if next == nil && filepath.Base(c.dbPath) != PreferredDBName() {
		if err := checkMarkerTarget(filepath.Join(filepath.Dir(c.dbPath), PreferredDBName())); err != nil {
			return ConeChange{}, fmt.Errorf("checkout.SetCone: %w", err)
		}
	}
	change := plan.change()
	names := append(append([]string(nil), change.Materialized...), change.Dematerialized...)
	saved, err := c.readFilesUndo(names)
	if err != nil {
		return ConeChange{}, fmt.Errorf("checkout.SetCone: %w", err)
	}
	oldName := filepath.Base(c.dbPath)
	if next != nil {
		if err := c.moveMarker(sparseDBName); err != nil {
			return ConeChange{}, fmt.Errorf("checkout.SetCone: %w", err)
		}
	}
	tx, err := c.db.Begin()
	if err == nil {
		err = c.applyConeChange(tx, vid, next, plan)
		if err == nil {
			err = tx.Commit()
		} else {
			err = errors.Join(err, tx.Rollback())
		}
	}
	if err != nil {
		restoreErr := c.rollBackUpdate(saved)
		if restoreErr == nil && filepath.Base(c.dbPath) != oldName {
			restoreErr = c.moveMarker(oldName)
		}
		return ConeChange{}, fmt.Errorf("checkout.SetCone: %w", errors.Join(err, restoreErr))
	}
	if next == nil {
		if err := c.moveMarker(PreferredDBName()); err != nil {
			if filepath.Base(c.dbPath) == PreferredDBName() {
				if moveErr := c.moveMarker(sparseDBName); moveErr != nil {
					c.cone = nil
					return change, fmt.Errorf("checkout.SetCone: dense transition completed but marker durability failed: %w", errors.Join(err, moveErr))
				}
			}
			metadataErr := c.restoreConeMetadata(vid, plan)
			restoreErr := c.rollBackUpdate(saved)
			return ConeChange{}, fmt.Errorf("checkout.SetCone: %w", errors.Join(err, metadataErr, restoreErr))
		}
	}
	c.cone = next
	return change, nil
}

func (c *Checkout) restoreConeMetadata(vid libfossil.FslID, plan conePlan) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range plan.entering {
		if r.ID <= 0 {
			if _, err := tx.Exec("DELETE FROM vfile WHERE vid=? AND pathname=?", int64(vid), r.Pathname); err != nil {
				return err
			}
		} else if _, err := tx.Exec("UPDATE vfile SET mtime = ? WHERE vid = ? AND pathname = ?", plan.mtimes[r.Pathname], int64(vid), r.Pathname); err != nil {
			return err
		}
	}
	for _, r := range plan.leaving {
		if _, err := insertManagedRow(tx, vid, r); err != nil {
			return err
		}
	}
	if err := ensureSparsePending(tx); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM sparse_pending"); err != nil {
		return err
	}
	for _, outcome := range plan.pending {
		if err := outcome.save(tx); err != nil {
			return err
		}
	}
	if err := writeCone(tx, c.cone); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Checkout) applyConeChange(tx *sql.Tx, vid libfossil.FslID, next *cone.Cone, plan conePlan) error {
	if err := ensureSparsePending(tx); err != nil {
		return err
	}
	for _, r := range plan.entering {
		id, err := insertManagedRow(tx, vid, r)
		if err != nil {
			return err
		}
		if !r.IsRemoved() {
			mtime, err := c.extractSingleFile(r.Pathname, r.MRID, r.IsExe, r.IsLink, false)
			if err != nil {
				return err
			}
			if err := c.recordMTime(tx, vid, r.Pathname, r.MRID, mtime); err != nil {
				return err
			}
		}
		if step, ok := plan.merges[r.Pathname]; ok {
			step.row.ID = id
			if err := c.applyMergeStep(tx, vid, step); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM sparse_pending WHERE vid=? AND pathname=?", int64(vid), r.Pathname); err != nil {
			return err
		}
	}
	for _, r := range plan.leaving {
		if err := c.dematerialize(r.Pathname); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM vfile WHERE id=?", r.ID); err != nil {
			return err
		}
	}
	return writeCone(tx, next)
}

// planConeChange sorts version vid's record into the rows a change to next
// brings into the cone and takes out of it, refusing a change that would
// lose work.
func (c *Checkout) planConeChange(vid libfossil.FslID, next *cone.Cone) (conePlan, error) {
	if err := c.refreshChanged(vid); err != nil {
		return conePlan{}, err
	}
	rows, err := c.effectiveRows(vid)
	if err != nil {
		return conePlan{}, err
	}
	plan := conePlan{mtimes: make(map[string]int64), merges: make(map[string]mergeStep)}
	plan.pending, err = c.snapshotPending()
	if err != nil {
		return conePlan{}, err
	}
	var atRisk []string
	for _, r := range rows {
		was, now := c.covers(r.Pathname), next == nil || next.Covers(r.Pathname)
		switch {
		case was && !now:
			changed, err := c.rowChanged(r)
			if err != nil {
				return conePlan{}, err
			}
			if changed {
				atRisk = append(atRisk, r.Pathname)
			}
			plan.leaving = append(plan.leaving, r)
		case !was && now:
			if !r.IsRemoved() {
				if err := c.checkEntering(r); err != nil {
					return conePlan{}, err
				}
			}
			for _, outcome := range plan.pending {
				if outcome.row.Pathname == r.Pathname && outcome.unresolved != 0 {
					step, err := c.planEnteringConflict(vid, r, outcome)
					if err != nil {
						return conePlan{}, err
					}
					plan.merges[r.Pathname] = step
				}
			}
			var mtime int64
			if r.ID > 0 {
				if err := c.db.QueryRow("SELECT COALESCE(mtime,0) FROM vfile WHERE id = ?", r.ID).Scan(&mtime); err != nil {
					return conePlan{}, err
				}
			}
			plan.mtimes[r.Pathname] = mtime
			plan.entering = append(plan.entering, r)
		}
	}
	if len(atRisk) > 0 {
		return conePlan{}, fmt.Errorf("files leaving the cone have local changes: %s",
			strings.Join(atRisk, ", "))
	}
	return plan, nil
}

// rowChanged reports whether a managed row holds work a narrowing would
// discard: any change vfile records, a pending merge, or a missing file.
func (c *Checkout) rowChanged(r vfile.Row) (bool, error) {
	missing, err := c.isMissing(r)
	if err != nil {
		return false, err
	}
	return vfile.Classify(r, missing) != ChangeNone || r.PendingMerge(), nil
}

// checkEntering refuses to materialize r over an untracked file of
// different content. Pending record-only merge bytes belong to mrid and are
// materialized as pending work, including conflicts that need resolution.
func (c *Checkout) checkEntering(r vfile.Row) error {
	if r.MRID <= 0 {
		return fmt.Errorf("file %s outside the cone has no recorded content", r.Pathname)
	}
	uuid, err := blobUUID(c.repo.DB(), libfossil.FslID(r.MRID))
	if err != nil {
		return err
	}
	clobber, err := c.wouldClobber(r.Pathname, uuid)
	if err != nil {
		return err
	}
	if clobber {
		return fmt.Errorf("untracked file %s is in the way of a file entering the cone", r.Pathname)
	}
	return nil
}

func (p conePlan) change() ConeChange {
	var out ConeChange
	for _, r := range p.entering {
		out.Materialized = append(out.Materialized, r.Pathname)
	}
	for _, r := range p.leaving {
		out.Dematerialized = append(out.Dematerialized, r.Pathname)
	}
	sort.Strings(out.Materialized)
	sort.Strings(out.Dematerialized)
	return out
}

// materialize writes r's committed content to disk and records the mtime
// the write gave it, as Extract does.
func (c *Checkout) materialize(vid libfossil.FslID, r vfile.Row) error {
	mtime, err := c.extractSingleFile(r.Pathname, r.MRID, r.IsExe, r.IsLink, false)
	if err != nil {
		return err
	}
	return c.recordMTime(c.db, vid, r.Pathname, r.RID, mtime)
}

// dematerialize removes name from disk, and then each parent directory the
// removal left empty, up to the checkout root.
func (c *Checkout) dematerialize(name string) error {
	full, err := c.safePath(name)
	if err != nil {
		return err
	}
	if err := c.env.Storage.Remove(full); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	// Bounded by the components of name.
	for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
		full, err := c.safePath(dir)
		if err != nil {
			return err
		}
		entries, err := c.env.Storage.ReadDir(full)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", dir, err)
		}
		if len(entries) > 0 {
			return nil
		}
		if err := c.env.Storage.Remove(full); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
	}
	return nil
}
