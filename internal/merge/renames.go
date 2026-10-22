package merge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/path"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// LocalFile is one of the checkout's files as its vfile row records it.
type LocalFile struct {
	Name string // pathname
	Orig string // origname: the name before a pending rename, or ""
	// Added is a file with no committed version (rid 0) or one a merge
	// added (chnged 3 or 5); it is never matched to another version's
	// file by name.
	Added bool
}

// MatchInput is everything fossil's merge_cmd fills its fv table from: the
// checkout's files (V), the pivot's (P) and the merged-in version's (M),
// and the renames from the name pivot N to each.
type MatchInput struct {
	Local                      []LocalFile // in vfile order
	Pivot, Merged              []string
	ToLocal, ToPivot, ToMerged []path.NameChange
	// NamePivotDiffers is nid!=pid, when fossil fills in names a rename
	// left unmatched.
	NamePivotDiffers bool
	// PivotNotAncestor is vAncestor 'n': a --baseline P that is not an
	// ancestor of V.
	PivotNotAncestor bool
}

// FileMatch is one row of fossil's fv table: one file as it is named in
// each version. Local indexes MatchInput.Local, -1 when the checkout does
// not have the file (idv 0). Pivot and Merged are fnp and fnm, "" when
// NULL; a name the version does not have means the version lacks the file.
type FileMatch struct {
	Local         int
	Pivot, Merged string
}

// fv columns, as merge_cmd names them.
const (
	fvV = iota // fn
	fvP        // fnp
	fvM        // fnm
	fvN        // fnn
	fvColumns
)

// fvRow is a row of fv; "" is NULL.
type fvRow struct {
	col   [fvColumns]string
	local int
	gone  bool
}

// fvTable is fossil's fv table: rows in rowid order, every name column
// UNIQUE (NULLs apart).
type fvTable struct {
	rows []*fvRow
	idx  [fvColumns]map[string]*fvRow
}

func newFVTable() *fvTable {
	t := &fvTable{}
	for c := range t.idx {
		t.idx[c] = map[string]*fvRow{}
	}
	return t
}

// insert adds r, as INSERT OR IGNORE, or with replace as REPLACE INTO:
// either way a row holding one of r's names conflicts.
func (t *fvTable) insert(r *fvRow, replace bool) bool {
	for c, v := range r.col {
		h := t.idx[c][v]
		if v == "" || h == nil {
			continue
		}
		if !replace {
			return false
		}
		t.remove(h)
	}
	t.rows = append(t.rows, r)
	for c, v := range r.col {
		if v != "" {
			t.idx[c][v] = r
		}
	}
	return true
}

func (t *fvTable) remove(r *fvRow) {
	r.gone = true
	for c, v := range r.col {
		if v != "" && t.idx[c][v] == r {
			delete(t.idx[c], v)
		}
	}
}

// set is UPDATE OR IGNORE of one column of r: false, changing nothing,
// when another row holds the name.
func (t *fvTable) set(r *fvRow, c int, v string) bool {
	if v == "" {
		panic("merge.fvTable.set: NULL name")
	}
	if r.col[c] == v {
		return true
	}
	if t.idx[c][v] != nil {
		return false
	}
	if old := r.col[c]; old != "" {
		delete(t.idx[c], old)
	}
	r.col[c] = v
	t.idx[c][v] = r
	return true
}

// live is the rows not deleted, in rowid order.
func (t *fvTable) live() []*fvRow {
	out := make([]*fvRow, 0, len(t.rows))
	for _, r := range t.rows {
		if !r.gone {
			out = append(out, r)
		}
	}
	return out
}

// addRenames is merge.c's add_renames: a row per name N's file has in the
// version, or that name set on the row already following the N name.
func (t *fvTable) addRenames(c int, changes []path.NameChange) error {
	for _, ch := range changes {
		r := &fvRow{local: -1}
		r.col[c], r.col[fvN] = ch.To, ch.From
		if t.insert(r, false) {
			continue
		}
		h := t.idx[fvN][ch.From]
		if h != nil && !t.set(h, c, ch.To) {
			// A plain UPDATE: fossil's merge stops on the constraint.
			return fmt.Errorf("merge: renames: UNIQUE constraint failed: %s is %s and %s", ch.From, ch.To, h.col[c])
		}
	}
	return nil
}

func coalesce(names ...string) string {
	for _, n := range names {
		if n != "" {
			return n
		}
	}
	return ""
}

func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// MatchFiles matches each file across the checkout, the pivot and the
// merged-in version, following renames, as fossil's merge_cmd fills its fv
// table (merge.c, "Compute name changes from N to V, P, and M" through
// "Add files found in M"). Each file of each version is in exactly one
// match. A rename chain the name pivot does not see, or names swapped
// between it and another version, match by name instead, as fossil's do.
func MatchFiles(in MatchInput) ([]FileMatch, error) {
	t := newFVTable()
	if err := t.addRenames(fvV, in.ToLocal); err != nil {
		return nil, err
	}
	if err := t.addRenames(fvP, in.ToPivot); err != nil {
		return nil, err
	}
	if err := t.addRenames(fvM, in.ToMerged); err != nil {
		return nil, err
	}
	pivot, merged := nameSet(in.Pivot), nameSet(in.Merged)
	local := make(map[string]bool, len(in.Local))
	for _, f := range in.Local {
		local[f.Name] = true
	}
	if in.NamePivotDiffers {
		t.fillIn(local, pivot, merged)
	}

	t.addLocal(in)

	// Add files found in P.
	for _, r := range t.live() {
		if r.col[fvP] != "" {
			continue
		}
		v := r.col[fvN]
		if v == "" && r.local >= 0 {
			v = coalesce(in.Local[r.local].Orig, in.Local[r.local].Name)
		}
		if v != "" {
			t.set(r, fvP, v)
		}
	}
	for _, name := range in.Pivot {
		r := &fvRow{local: -1}
		r.col[fvP] = name
		t.insert(r, false)
	}

	// Add files found in M.
	for _, r := range t.live() {
		if r.col[fvM] == "" && r.col[fvP] != "" {
			t.set(r, fvM, r.col[fvP])
		}
	}
	for _, name := range in.Merged {
		r := &fvRow{local: -1}
		r.col[fvM] = name
		t.insert(r, false)
	}

	rows := t.live()
	out := make([]FileMatch, len(rows))
	for i, r := range rows {
		out[i] = FileMatch{Local: r.local, Pivot: r.col[fvP], Merged: r.col[fvM]}
	}
	return out, nil
}

// addLocal is merge_cmd's "Add files found in V": a row per checkout file,
// taking over the row that follows its committed name. The REPLACE's
// SELECT reads fv as it was before it.
func (t *fvTable) addLocal(in MatchInput) {
	for _, r := range t.live() {
		if r.col[fvV] != "" {
			continue
		}
		from := r.col[fvP]
		if in.PivotNotAncestor {
			from = ""
		}
		if v := coalesce(from, r.col[fvN]); v != "" {
			t.set(r, fvV, v)
		}
	}
	matched := make([]*fvRow, len(in.Local))
	for i, f := range in.Local {
		if h := t.idx[fvV][coalesce(f.Orig, f.Name)]; h != nil && !f.Added {
			copied := *h
			matched[i] = &copied
		}
	}
	for i, f := range in.Local {
		r := &fvRow{local: i}
		if m := matched[i]; m != nil {
			r.col = m.col
		}
		r.col[fvV] = f.Name
		t.insert(r, true)
	}
}

// fillIn is merge_cmd's name fill-in for nid!=pid (forum post 549700437b):
// a name missing from one slot is taken from another where that version
// has a file of that name. Like fossil's, it does not see a name changed
// twice between N and V or M, nor names swapped.
func (t *fvTable) fillIn(local, pivot, merged map[string]bool) {
	rows := t.live()
	for _, r := range rows {
		if n := r.col[fvN]; r.col[fvP] == "" && pivot[n] {
			t.set(r, fvP, n)
		}
	}
	for _, r := range rows {
		if n := coalesce(r.col[fvP], r.col[fvN]); r.col[fvV] == "" && local[n] {
			t.set(r, fvV, n)
		}
	}
	for _, r := range rows {
		if n := coalesce(r.col[fvP], r.col[fvN]); r.col[fvM] == "" && merged[n] {
			t.set(r, fvM, n)
		}
	}
	for _, r := range rows {
		if r.col[fvP] != "" {
			continue
		}
		// vfile.pathname IN (fv.fnm, fv.fn): SQLite takes one; fnm first.
		for _, n := range []string{r.col[fvM], r.col[fvV]} {
			if pivot[n] {
				t.set(r, fvP, n)
				break
			}
		}
	}
	for _, r := range rows {
		if n := r.col[fvM]; r.col[fvV] == "" && local[n] {
			t.set(r, fvV, n)
		}
	}
	for _, r := range rows {
		if n := r.col[fvV]; r.col[fvM] == "" && merged[n] {
			t.set(r, fvM, n)
		}
	}
}

// RenameQuery is a merge as merge_cmd sees it: V, the checkout's version;
// P, the pivot the merge diffs against; M, the version merged in; and each
// one's files. Baseline is a P named by the caller, as --baseline names it.
type RenameQuery struct {
	V, P, M       libfossil.FslID
	Baseline      bool
	Local         []LocalFile
	Pivot, Merged []string
}

// FollowRenames matches each file across a merge's versions, following
// the renames between them as fossil's merge does: from the name pivot N
// (NamePivot) to V, P and M (path.FilenameChanges, one way), then
// MatchFiles.
func FollowRenames(ctx context.Context, r *repo.Repo, q RenameQuery) ([]FileMatch, error) {
	if ctx == nil || r == nil {
		panic("merge.FollowRenames: nil context or repo")
	}
	if q.V <= 0 || q.P <= 0 || q.M <= 0 {
		panic("merge.FollowRenames: versions must be positive")
	}
	nid, err := NamePivot(ctx, r, q.M, q.V, q.P)
	if err != nil {
		return nil, err
	}
	in := MatchInput{Local: q.Local, Pivot: q.Pivot, Merged: q.Merged, NamePivotDiffers: nid != q.P}
	for _, to := range []struct {
		rid     libfossil.FslID
		changes *[]path.NameChange
	}{{q.V, &in.ToLocal}, {q.P, &in.ToPivot}, {q.M, &in.ToMerged}} {
		if *to.changes, err = path.FilenameChanges(r.DB(), nid, to.rid, false); err != nil {
			return nil, err
		}
	}
	if q.Baseline {
		ancestor, err := baselineIsAncestor(r.DB(), q.V, nid, q.P)
		if err != nil {
			return nil, err
		}
		in.PivotNotAncestor = !ancestor
	}
	return MatchFiles(in)
}

// NamePivot is merge_cmd's name pivot nid: the pivot of M and V over
// primary links alone (pivot_find(1)), and when that is not P, the same
// pivot of it and P. With none it is P.
func NamePivot(ctx context.Context, r *repo.Repo, mid, vid, pid libfossil.FslID) (libfossil.FslID, error) {
	if mid <= 0 || vid <= 0 || pid <= 0 {
		panic("merge.NamePivot: versions must be positive")
	}
	nid, err := primaryPivot(ctx, r, mid, vid)
	if err != nil {
		return 0, err
	}
	if nid != 0 && nid != pid {
		if nid, err = primaryPivot(ctx, r, nid, pid); err != nil {
			return 0, err
		}
	}
	if nid == 0 {
		return pid, nil
	}
	return nid, nil
}

func primaryPivot(ctx context.Context, r *repo.Repo, primary, secondary libfossil.FslID) (libfossil.FslID, error) {
	p, err := FindPivot(ctx, r, PivotQuery{
		Primary: primary, Secondaries: []libfossil.FslID{secondary}, IgnoreMerges: true,
	})
	if errors.Is(err, ErrNoCommonAncestor) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("name pivot: %w", err)
	}
	return p.RID, nil
}

// baselineIsAncestor is merge_cmd's vAncestor test for --baseline: whether
// P is reached walking V's ancestry, never through N nor out of P, with
// fossil's query as it stands.
func baselineIsAncestor(q interface {
	QueryRow(string, ...any) *sql.Row
}, vid, nid, pid libfossil.FslID) (bool, error) {
	var found bool
	err := q.QueryRow(`WITH RECURSIVE ancestor(id) AS (
		  VALUES(?)
		  UNION
		  SELECT pid FROM plink, ancestor
		   WHERE cid=ancestor.id AND pid!=? AND cid!=?)
		SELECT EXISTS(SELECT 1 FROM ancestor WHERE id=?)`,
		int64(vid), int64(nid), int64(pid), int64(pid)).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("baseline ancestry: %w", err)
	}
	return found, nil
}
