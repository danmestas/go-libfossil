package checkout

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// MergeAction is what Merge did, or would do, to one file.
type MergeAction int

const (
	// MergeUpdated: only the merged-in version changed the file, so its
	// content replaced the checkout's.
	MergeUpdated MergeAction = iota + 1
	// MergeMerged: both sides changed the file and the strategy combined
	// them cleanly.
	MergeMerged
	// MergeConflict: both sides changed the file and the merged content,
	// conflict markers included, was written into it.
	MergeConflict
	// MergeKept: the file could not be merged and the checkout's copy was
	// left as it is. MergeFile.Reason says why.
	MergeKept
	// MergeForked: the conflict-fork strategy recorded every version in the
	// repository's conflict table and left the checkout's copy as it is.
	MergeForked
	// MergeAdded: the file is new in the merged-in version.
	MergeAdded
	// MergeDeleted: the merged-in version deleted the file and the checkout
	// had not changed it.
	MergeDeleted
	// MergeMode: only the merged-in version changed the file's executable
	// bit, so its bit was taken.
	MergeMode
	// MergeRenamed: the merged-in version renamed the file and changed
	// nothing else the merge takes, so it was only renamed.
	MergeRenamed
)

// MergeOpts configures Merge.
type MergeOpts struct {
	Version  libfossil.FslID // the checkin to merge in; required
	Strategy string          // strategy for every file; "" picks one per file
	DryRun   bool            // plan the merge and report it, changing nothing
	// Baseline, when set, is the common ancestor to diff against instead of
	// the one Merge finds, as fossil's merge --baseline; it must be held.
	Baseline libfossil.FslID
}

// MergeFile reports one file Merge acted on. Name is the file's name in
// the checkout before the merge, or in the merged-in version for an added
// file.
type MergeFile struct {
	Name      string
	Action    MergeAction
	Strategy  string // the strategy that merged the file, when one ran
	Reason    string // why the file was kept, for MergeKept
	RenamedTo string // the name the merge renamed the file to, if it did
}

// MergeResult reports a merge. Files is sorted by name and is empty when
// there was nothing to merge.
type MergeResult struct {
	Ancestor libfossil.FslID // the common ancestor the merge diffed against
	Files    []MergeFile
}

// Merge merges another checkin into the checkout, the way fossil's merge
// command does, leaving the result uncommitted. Changes the merged-in
// version made since the common ancestor are applied to the working files;
// a file both sides changed is merged by its strategy, with conflict markers
// written into the file when the strategy cannot reconcile them. A conflict
// is not recorded anywhere else: the file is in conflict while it holds
// markers (see Conflicts). The merged-in version becomes the next commit's
// merge parent.
//
// The checkout records the merge as fossil does, so fossil reads it too:
// a taken-over file gets chnged=2 and mrid set to the merged-in content, an
// added file chnged=3, a merged file a per-file vmerge row, and the merge
// itself a vmerge row with id 0.
//
// Merge refuses, changing nothing, when the repository is missing content
// it needs or when an added file would overwrite an untracked file. In a
// sparse checkout, uncovered changes are recorded without touching disk. Where
// fossil relies on its undo copy, Merge keeps the checkout's file instead:
// a file edited here but deleted in the merged-in version is kept and
// reported, not deleted.
//
// Renames are followed as fossil's merge follows them (merge.FollowRenames):
// a file is matched across the versions by its name in each, so a change
// made under one name is merged into the file under another. A file the
// merged-in version renamed and the checkout did not is renamed, and keeps
// whatever changes the checkout made; a file the checkout renamed keeps its
// name. Where fossil would rename a file over one the checkout tracks and
// keeps, or over an untracked file, Merge refuses, as it does an add.
//
// opts.Strategy names the strategy for every file; empty picks one per file
// from the repository's .libfossil-merge rules and merge-strategy setting,
// falling back to three-way. Merging the checkout's own version or one of
// its ancestors does nothing and returns an empty result.
func (c *Checkout) Merge(opts MergeOpts) (MergeResult, error) {
	if c == nil {
		panic("checkout.Merge: nil *Checkout")
	}
	if opts.Version <= 0 {
		return MergeResult{}, fmt.Errorf("checkout.Merge: invalid version rid %d", opts.Version)
	}

	vid, _, err := c.Version()
	if err != nil {
		return MergeResult{}, fmt.Errorf("checkout.Merge: %w", err)
	}
	if vid <= 0 {
		return MergeResult{}, fmt.Errorf("checkout.Merge: nothing is checked out")
	}
	if c.cone != nil {
		var pending bool
		if err := c.db.QueryRow("SELECT EXISTS(SELECT 1 FROM vmerge WHERE id<=0)").Scan(&pending); err != nil {
			return MergeResult{}, fmt.Errorf("checkout.Merge: pending merge: %w", err)
		}
		if pending {
			return MergeResult{}, fmt.Errorf("checkout.Merge: a merge is already pending; resolve and commit it, or undo it")
		}
	}
	if opts.Version == vid {
		return MergeResult{}, nil
	}
	base, err := c.mergeBase(vid, opts)
	if err != nil && !errors.Is(err, merge.ErrNoCommonAncestor) {
		return MergeResult{}, fmt.Errorf("checkout.Merge: common ancestor: %w", err)
	}
	pivot := base.RID
	// draft-fossil-horizon-00 §5.5: never merge against a base the store
	// does not hold, nor one found by a walk that crossed the horizon, and
	// name the horizon when that is why.
	if cerr := content.MergeBaseCrossing(c.repo.DB(), opts.Version, pivot, base.Unheld); cerr != nil {
		return MergeResult{}, fmt.Errorf("checkout.Merge: %w", cerr)
	}
	if err != nil {
		return MergeResult{}, fmt.Errorf("checkout.Merge: common ancestor: %w", err)
	}
	if pivot == opts.Version {
		return MergeResult{Ancestor: pivot}, nil // already part of the checkout
	}

	// Which files are edited decides between taking the merged-in version
	// and merging, so chnged must be current. As fossil's merge, refuse
	// something other than a file where a tracked file belongs.
	if err := c.ScanChanges(ScanENotFile); err != nil {
		return MergeResult{}, fmt.Errorf("checkout.Merge: %w", err)
	}
	plan := mergePlan{vid: vid, pivot: pivot, mid: opts.Version, baseline: opts.Baseline > 0}
	if err := c.planMerge(&plan, opts.Strategy); err != nil {
		return MergeResult{}, fmt.Errorf("checkout.Merge: %w", err)
	}
	if err := c.checkMergeTargets(plan); err != nil {
		return MergeResult{}, fmt.Errorf("checkout.Merge: %w", err)
	}
	if !opts.DryRun {
		if err := c.applyMerge(plan); err != nil {
			return MergeResult{}, fmt.Errorf("checkout.Merge: %w", err)
		}
	}
	return plan.result(), nil
}

// mergeBase is the common ancestor the merge diffs against, as fossil's
// merge_cmd chooses it (merge.c, pivot_set_primary(mid) through pid =
// pivot_find(0)): the pivot of the merged-in version, primary, and the
// checkout's version together with every check-in already merged into it,
// the vmerge rows with id 0. opts.Baseline overrides it, as --baseline does.
// The name pivot renames are followed from is merge.FollowRenames'.
func (c *Checkout) mergeBase(vid libfossil.FslID, opts MergeOpts) (merge.Pivot, error) {
	if opts.Baseline > 0 {
		return merge.Pivot{RID: opts.Baseline}, nil
	}
	secondaries := []libfossil.FslID{vid}
	rows, err := c.db.Query("SELECT merge FROM vmerge WHERE id=0")
	if err != nil {
		return merge.Pivot{}, fmt.Errorf("pending merges: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return merge.Pivot{}, fmt.Errorf("pending merges: %w", err)
		}
		secondaries = append(secondaries, libfossil.FslID(rid))
	}
	if err := rows.Err(); err != nil {
		return merge.Pivot{}, fmt.Errorf("pending merges: %w", err)
	}
	return merge.FindPivot(context.Background(), c.repo, merge.PivotQuery{Primary: opts.Version, Secondaries: secondaries})
}

// mergePlan is a merge worked out in full before anything is changed.
type mergePlan struct {
	vid, pivot, mid libfossil.FslID
	baseline        bool        // pivot was given, not found
	rows            []vfile.Row // the checkout's files
	steps           []mergeStep // sorted by name
}

// mergeStep is one file's part of a merge.
type mergeStep struct {
	file       MergeFile
	row        vfile.Row // the checkout's row; zero for an added file
	merged     mergeSide // the merged-in version of the file
	content    []byte    // what to write; nil when the file is not written
	exe        bool      // the checkout file's executable bit after the merge
	fork       forkSides // which versions have the file, for a fork record
	recordOnly bool      // the shared cone decision; no filesystem access for this step
	pivotRID   libfossil.FslID
}

// forkSides says which of the three versions a conflict-fork record names.
type forkSides struct {
	base, local, remote bool
}

// mergeSide is one version's copy of a file. rid 0 means the version does
// not have the file.
type mergeSide struct {
	rid  libfossil.FslID
	uuid string
	exe  bool
	link bool
}

func (p mergePlan) result() MergeResult {
	files := make([]MergeFile, len(p.steps))
	for i, s := range p.steps {
		files[i] = s.file
	}
	return MergeResult{Ancestor: p.pivot, Files: files}
}

// mergeKind is how a file's three versions combine.
type mergeKind int

const (
	mergeSkip        mergeKind = iota // the merged-in version did not change it
	mergeTake                         // changed only there: take its content
	mergeThreeWay                     // changed on both sides: merge
	mergeAdd                          // new there
	mergeDelete                       // deleted there, untouched here
	mergeKeepEdited                   // deleted there, edited here
	mergeKeepAbsent                   // changed there, not tracked here
	mergeKeepRemoved                  // changed there, removal pending here
	mergeMode                         // only the executable bit changed there
)

// classifyMerge decides how a file combines, from the checkout's row (nil
// when the checkout does not track the file), whether the checkout changed
// it, and the ancestor's and merged-in version's copies. It follows the
// cases of fossil's merge_cmd.
func classifyMerge(row *vfile.Row, p, m mergeSide) mergeKind {
	if m.rid == p.rid {
		if row != nil && !row.IsRemoved() && m.rid != 0 && takesMergedExe(*row, p, m) {
			return mergeMode
		}
		return mergeSkip
	}
	if row == nil {
		if p.rid == 0 {
			return mergeAdd
		}
		if m.rid == 0 {
			return mergeSkip // deleted on both sides
		}
		return mergeKeepAbsent
	}
	if row.IsRemoved() {
		if m.rid == 0 {
			return mergeSkip
		}
		return mergeKeepRemoved
	}
	unchanged := !row.ContentChanged()
	if m.rid == 0 {
		if unchanged && libfossil.FslID(row.RID) == p.rid {
			return mergeDelete
		}
		return mergeKeepEdited
	}
	if unchanged && libfossil.FslID(row.RID) == p.rid {
		return mergeTake
	}
	if unchanged && libfossil.FslID(row.RID) == m.rid {
		return mergeSkip // the checkout already has the merged-in content
	}
	return mergeThreeWay
}

// takesMergedExe reports whether the merged-in version changed the file's
// executable bit and the checkout did not, so the merge takes its bit, as
// fossil's merge does for a file whose content it leaves alone. (For a file
// it merges, fossil always takes the merged-in bit; go-libfossil keeps a
// change the checkout made to it.)
func takesMergedExe(row vfile.Row, p, m mergeSide) bool {
	if m.exe == p.exe {
		return false
	}
	return (row.IsExe > 0) == p.exe
}

// mergedExe is the executable bit a merged file ends up with.
func mergedExe(row vfile.Row, p, m mergeSide) bool {
	if takesMergedExe(row, p, m) {
		return m.exe
	}
	return row.IsExe > 0
}

// keepReasons explains each kind of file Merge keeps without strategy.
var keepReasons = map[mergeKind]string{
	mergeKeepEdited:  "edited here, deleted in the merged-in version",
	mergeKeepAbsent:  "changed in the merged-in version, not in this checkout",
	mergeKeepRemoved: "changed in the merged-in version, removed here",
}

// planMerge fills plan.steps with every file the merge touches. It reads all
// the content the merge needs, so a merge it cannot finish fails here.
func (c *Checkout) planMerge(plan *mergePlan, strategyName string) error {
	if plan.vid <= 0 || plan.mid <= 0 {
		panic("checkout.planMerge: versions must be positive")
	}
	choose, err := c.mergeStrategyChooser(plan.vid, strategyName)
	if err != nil {
		return err
	}
	rows, err := c.effectiveRows(plan.vid)
	if err != nil {
		return err
	}
	plan.rows = rows
	ancestor, err := c.mergeSides(plan.pivot)
	if err != nil {
		return err
	}
	merged, err := c.mergeSides(plan.mid)
	if err != nil {
		return err
	}
	local := make([]merge.LocalFile, len(rows))
	for i, r := range rows {
		local[i] = merge.LocalFile{
			Name: r.Pathname, Orig: r.RenamedFrom(),
			Added: r.IsAdded() || r.Chnged == 3 || r.Chnged == 5,
		}
	}
	matches, err := merge.FollowRenames(context.Background(), c.repo, merge.RenameQuery{
		V: plan.vid, P: plan.pivot, M: plan.mid, Baseline: plan.baseline,
		Local: local, Pivot: sortedNames(ancestor), Merged: sortedNames(merged),
	})
	if err != nil {
		return err
	}

	for _, match := range matches {
		var row *vfile.Row
		if match.Local >= 0 {
			row = &rows[match.Local]
		}
		p, m := ancestor[match.Pivot], merged[match.Merged]
		kind := classifyMerge(row, p, m)
		renameTo := mergeRenameTo(row, match, p, m)
		if kind == mergeSkip && renameTo == "" {
			continue
		}
		name := match.Pivot
		switch {
		case row != nil:
			name = row.Pathname
		case m.rid > 0:
			name = match.Merged
		}
		var step mergeStep
		if kind == mergeSkip {
			step = c.planRenameOnly(*row, p, m)
		} else if step, err = c.planStep(name, kind, row, p, m, choose); err != nil {
			return err
		}
		step.file.RenamedTo = renameTo
		plan.steps = append(plan.steps, step)
	}
	sort.Slice(plan.steps, func(i, j int) bool {
		return plan.steps[i].file.Name < plan.steps[j].file.Name
	})
	return nil
}

// mergeRenameTo is the name the merge renames the checkout's file to, or
// "": a file the merged-in version renamed and the checkout did not, as
// fossil's merge picks them (fnp=fn AND fnm!=fnp, every version having it).
func mergeRenameTo(row *vfile.Row, match merge.FileMatch, p, m mergeSide) string {
	if row == nil || p.rid <= 0 || m.rid <= 0 {
		return ""
	}
	if match.Pivot != row.Pathname || match.Merged == match.Pivot {
		return ""
	}
	return match.Merged
}

// planRenameOnly is the step for a file the merge only renames.
func (c *Checkout) planRenameOnly(row vfile.Row, p, m mergeSide) mergeStep {
	return mergeStep{
		file:       MergeFile{Name: row.Pathname, Action: MergeRenamed},
		row:        row,
		merged:     m,
		exe:        mergedExe(row, p, m),
		recordOnly: !c.covers(row.Pathname),
		pivotRID:   p.rid,
	}
}

// sortedNames is a version's file names, sorted.
func sortedNames(sides map[string]mergeSide) []string {
	names := make([]string, 0, len(sides))
	for name := range sides {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// mergeSides lists checkin rid's files by name. It refuses a checkin whose
// content the repository does not fully hold, as fossil's merge does.
func (c *Checkout) mergeSides(rid libfossil.FslID) (map[string]mergeSide, error) {
	files, blobRIDs, err := c.resolveFiles(rid, nil)
	if err != nil {
		return nil, err
	}
	sides := make(map[string]mergeSide, len(files))
	for i, f := range files {
		sides[f.Name] = mergeSide{
			rid:  blobRIDs[i],
			uuid: f.UUID,
			exe:  f.Perm == "x",
			link: f.Perm == "l",
		}
	}
	return sides, nil
}

// mergeStrategyChooser returns the strategy for each file: the named one for
// every file, or the repository's rules for each.
func (c *Checkout) mergeStrategyChooser(
	vid libfossil.FslID, name string,
) (func(string) (merge.Strategy, error), error) {
	if name != "" {
		strat, ok := merge.StrategyByName(name)
		if !ok {
			return nil, fmt.Errorf("unknown merge strategy %q", name)
		}
		return func(string) (merge.Strategy, error) { return strat, nil }, nil
	}
	resolver := merge.LoadResolver(c.repo, vid)
	return func(file string) (merge.Strategy, error) {
		picked := resolver.Resolve(file)
		strat, ok := merge.StrategyByName(picked)
		if !ok {
			return nil, fmt.Errorf("unknown merge strategy %q for %s", picked, file)
		}
		return strat, nil
	}, nil
}

// planStep works out one file's step: what happens to it and, for a file
// that is written, its new content.
func (c *Checkout) planStep(
	name string, kind mergeKind, row *vfile.Row, p, m mergeSide,
	choose func(string) (merge.Strategy, error),
) (mergeStep, error) {
	if kind == mergeSkip {
		panic("checkout.planStep: nothing to plan for " + name)
	}
	step := mergeStep{file: MergeFile{Name: name}, merged: m, recordOnly: !c.covers(name), pivotRID: p.rid}
	if row != nil {
		step.exe = mergedExe(*row, p, m)
	}
	step.fork = forkSides{
		base:   p.rid > 0,
		local:  row != nil && !row.IsRemoved(),
		remote: m.rid > 0,
	}
	if row != nil {
		step.row = *row
	}
	if step.recordOnly {
		if row != nil && !row.IsRemoved() && libfossil.FslID(row.MRID) == m.rid && (row.IsExe > 0) == m.exe && (row.IsLink > 0) == m.link {
			step.file.Action = MergeKept
			return step, nil
		}
		switch kind {
		case mergeTake:
			step.file.Action = MergeUpdated
		case mergeAdd:
			step.file.Action = MergeAdded
		case mergeDelete:
			step.file.Action = MergeDeleted
		case mergeMode:
			step.file.Action = MergeMode
		default:
			step.file.Action, step.file.Reason = MergeConflict, "outside both-changed path requires widening the sparse cone"
		}
		return step, nil
	}
	if reason, ok := keepReasons[kind]; ok {
		return step, keepOrFork(&step, reason, choose)
	}
	if m.link || p.link || step.row.IsLink > 0 {
		step.file.Action, step.file.Reason = MergeKept, "symlinks are not merged"
		return step, nil
	}

	var err error
	switch kind {
	case mergeTake, mergeAdd:
		step.file.Action = MergeUpdated
		if kind == mergeAdd {
			step.file.Action = MergeAdded
		}
		step.content, err = content.Expand(c.repo.DB(), m.rid)
	case mergeDelete:
		step.file.Action = MergeDeleted
	case mergeMode:
		step.file.Action = MergeMode
	case mergeThreeWay:
		err = c.planThreeWay(&step, p, choose)
	default:
		panic(fmt.Sprintf("checkout.planStep: unhandled merge kind %d", kind))
	}
	if err != nil {
		return mergeStep{}, fmt.Errorf("%s: %w", name, err)
	}
	return step, nil
}

// keepOrFork settles a file that cannot be merged: kept as it is, or, under
// the conflict-fork strategy, recorded as a fork so every version survives.
func keepOrFork(
	step *mergeStep, reason string, choose func(string) (merge.Strategy, error),
) error {
	if reason == "" {
		panic("checkout.keepOrFork: no reason for " + step.file.Name)
	}
	strat, err := choose(step.file.Name)
	if err != nil {
		return err
	}
	if strat.Name() == "conflict-fork" {
		step.file.Action, step.file.Strategy = MergeForked, strat.Name()
		return nil
	}
	step.file.Action, step.file.Reason = MergeKept, reason
	return nil
}

// planThreeWay merges the ancestor's copy, the working file and the merged-in
// copy with the file's strategy, and records the outcome in step.
func (c *Checkout) planThreeWay(
	step *mergeStep, p mergeSide, choose func(string) (merge.Strategy, error),
) error {
	if step.row.Pathname == "" {
		panic("checkout.planThreeWay: no checkout row for " + step.file.Name)
	}
	local, present, err := c.readWorkingFile(step.row)
	if err != nil {
		return err
	}
	if !present {
		return keepOrFork(step, "missing from the checkout", choose)
	}
	base := []byte{}
	if p.rid > 0 {
		if base, err = content.Expand(c.repo.DB(), p.rid); err != nil {
			return err
		}
	}
	remote, err := content.Expand(c.repo.DB(), step.merged.rid)
	if err != nil {
		return err
	}
	strat, err := choose(step.file.Name)
	if err != nil {
		return err
	}
	if !(merge.IsText(base) && merge.IsText(local) && merge.IsText(remote)) {
		// Fossil's merge refuses binary content: "cannot merge binary
		// file", and the checkout's copy stays, whatever strategy the file
		// would have had.
		step.file.Strategy = strat.Name()
		step.file.Action = MergeKept
		step.file.Reason = "it is a binary file"
		return nil
	}
	res, err := strat.Merge(base, local, remote)
	if err != nil {
		return err
	}

	step.file.Strategy = strat.Name()
	switch {
	case res.Clean:
		step.file.Action, step.content = MergeMerged, res.Content
	case strat.Name() == "conflict-fork":
		step.file.Action = MergeForked
	case merge.HasConflictMarkers(res.Content):
		step.file.Action, step.content = MergeConflict, res.Content
	default:
		step.file.Action = MergeKept
		step.file.Reason = "the " + strat.Name() + " strategy cannot merge it"
	}
	return nil
}

// readWorkingFile reads row's file from disk. present is false when it is
// missing or is not a regular file.
func (c *Checkout) readWorkingFile(row vfile.Row) (data []byte, present bool, err error) {
	if !c.covers(row.Pathname) {
		if row.IsRemoved() || row.MRID <= 0 {
			return nil, false, nil
		}
		data, err := content.Expand(c.repo.DB(), libfossil.FslID(row.MRID))
		return data, err == nil, err
	}
	missing, err := c.isMissing(row)
	if err != nil {
		return nil, false, err
	}
	if missing {
		return nil, false, nil
	}
	fullPath, err := c.safePath(row.Pathname)
	if err != nil {
		return nil, false, err
	}
	data, err = c.env.Storage.ReadFile(fullPath)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", fullPath, err)
	}
	if data == nil {
		data = []byte{}
	}
	return data, true, nil
}

// checkMergeTargets refuses, before anything is written, a merge that could
// not finish or would destroy something: an added file over an untracked
// file holding other content (fossil backs it up and overwrites it;
// go-libfossil has no undo copy, so it refuses, as Extract does), or a file
// the merge writes or deletes where something other than a regular file
// stands, or under a parent that is not a directory. Failing on those
// midway would leave files merged on disk with no record of the merge.
func (c *Checkout) checkMergeTargets(plan mergePlan) error {
	steps := plan.steps
	var unresolved []string
	for _, s := range steps {
		if s.recordOnly && s.file.Action == MergeConflict {
			unresolved = append(unresolved, s.file.Name)
		}
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("unresolved both-changed paths require widening and re-running the merge: %w", c.outsideCone("Merge", unresolved))
	}
	if err := c.checkMergeRenames(plan); err != nil {
		return err
	}
	for _, s := range steps {
		if !s.changesDisk() {
			continue
		}
		if err := c.checkMergeTarget(s.file.Name); err != nil {
			return err
		}
		if s.file.Action != MergeAdded {
			continue
		}
		clobber, err := c.wouldClobber(s.file.Name, s.merged.uuid)
		if err != nil {
			return err
		}
		if clobber {
			return fmt.Errorf(
				"merge would overwrite untracked file %s; move it aside first", s.file.Name,
			)
		}
	}
	return nil
}

// checkMergeRenames refuses a rename the merge cannot carry out without
// destroying something: onto a name the checkout tracks and keeps, or onto
// a file on disk that no step moves or deletes first (fossil overwrites
// either, relying on its undo copy). A rename into or out of a sparse cone
// is refused too: the file would change between materialized and not.
func (c *Checkout) checkMergeRenames(plan mergePlan) error {
	vacated := map[string]bool{}
	var outside []string
	for _, s := range plan.steps {
		if s.file.RenamedTo != "" || s.file.Action == MergeDeleted {
			vacated[s.file.Name] = true
		}
		if s.file.RenamedTo != "" && (!c.covers(s.file.Name) || !c.covers(s.file.RenamedTo)) {
			outside = append(outside, s.file.Name)
		}
	}
	if len(outside) > 0 {
		return fmt.Errorf("renames across the sparse cone require widening and re-running the merge: %w", c.outsideCone("Merge", outside))
	}
	tracked := make(map[string]vfile.Row, len(plan.rows))
	for _, r := range plan.rows {
		tracked[r.Pathname] = r
	}
	for _, s := range plan.steps {
		to := s.file.RenamedTo
		if to == "" || vacated[to] {
			continue
		}
		if r, ok := tracked[to]; ok && !r.IsRemoved() {
			return fmt.Errorf("merge would rename %s over %s, which this checkout tracks", s.file.Name, to)
		}
		if err := c.checkMergeTarget(to); err != nil {
			return err
		}
		full, err := c.safePath(to)
		if err != nil {
			return err
		}
		if _, err := c.env.Storage.Stat(full); err == nil {
			return fmt.Errorf("merge would rename %s over untracked file %s; move it aside first", s.file.Name, to)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", full, err)
		}
	}
	return nil
}

// changesDisk reports whether applying the step writes, deletes or changes
// the mode of its file.
func (s mergeStep) changesDisk() bool {
	if s.recordOnly {
		return false
	}
	switch s.file.Action {
	case MergeUpdated, MergeMerged, MergeConflict, MergeAdded, MergeDeleted, MergeMode:
		return true
	default:
		return false
	}
}

// checkMergeTarget refuses a path whose file is not a regular file, or one
// of whose parents is not a directory. Storage has no Lstat, so a symlink is
// judged by what it points to.
func (c *Checkout) checkMergeTarget(name string) error {
	fullPath, err := c.safePath(name)
	if err != nil {
		return err
	}
	info, err := c.env.Storage.Stat(fullPath)
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("cannot merge %s: it is not a regular file", name)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", fullPath, err)
	}
	// Walk the checkout-relative name, so the walk ends at the checkout root
	// whatever form c.dir takes.
	clean, err := c.CheckFilename(name)
	if err != nil {
		return err
	}
	for dir := path.Dir(filepath.ToSlash(clean)); dir != "."; dir = path.Dir(dir) {
		info, err := c.env.Storage.Stat(filepath.Join(c.dir, filepath.FromSlash(dir)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("cannot merge %s: %s is not a directory", name, dir)
		}
	}
	return nil
}

// applyMerge carries out a planned merge: each step's working file and
// checkout record, then the merge itself, in one checkout transaction; then
// the repository's fork records.
func (c *Checkout) applyMerge(plan mergePlan) error {
	if plan.mid <= 0 {
		panic("checkout.applyMerge: no merged-in version")
	}
	var names []string
	seen := map[string]bool{}
	touch := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, s := range plan.steps {
		if s.changesDisk() || s.file.RenamedTo != "" {
			touch(s.file.Name)
		}
		if s.file.RenamedTo != "" {
			touch(s.file.RenamedTo)
		}
	}
	saved, err := c.readFilesUndo(names)
	if err != nil {
		return err
	}
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := saved.save(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := c.applyMergeTx(tx, plan); err != nil {
		return errors.Join(err, tx.Rollback(), c.rollBackUpdate(saved))
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(fmt.Errorf("commit: %w", err), c.rollBackUpdate(saved))
	}
	return c.recordForks(plan)
}

func (c *Checkout) applyMergeTx(tx *sql.Tx, plan mergePlan) error {
	for _, s := range plan.steps {
		if err := c.applyMergeStep(tx, plan.vid, s); err != nil {
			return fmt.Errorf("%s: %w", s.file.Name, err)
		}
	}
	if err := c.applyMergeRenames(tx, plan); err != nil {
		return err
	}
	mhash, err := blobUUID(c.repo.DB(), plan.mid)
	if err != nil {
		return err
	}
	return insertVMerge(tx, 0, plan.mid, mhash)
}

// alwaysDiffers stands in for hashing a file known to differ from its
// baseline.
func alwaysDiffers() (bool, error) { return true, nil }

// applyMergeStep writes one file and records it the way fossil's merge
// does.
func (c *Checkout) applyMergeStep(tx *sql.Tx, vid libfossil.FslID, s mergeStep) error {
	if s.recordOnly {
		return c.applyRecordOnlyMerge(tx, vid, s)
	}
	if s.changesDisk() {
		if err := c.applyMergeDiskStep(s); err != nil {
			return err
		}
	}
	switch s.file.Action {
	case MergeUpdated:
		_, err := tx.Exec(`UPDATE vfile SET mtime=0, chnged=2, mrid=?, mhash=?, isexe=?
			WHERE id=?`, int64(s.merged.rid), s.merged.uuid, s.merged.exe, s.row.ID)
		return err
	case MergeAdded:
		_, err := tx.Exec(`INSERT INTO vfile(vid, chnged, deleted, rid, mrid, isexe, islink,
			pathname) VALUES(?, 3, 0, ?, ?, ?, 0, ?)`,
			int64(vid), int64(s.merged.rid), int64(s.merged.rid), s.merged.exe, s.file.Name)
		return err
	case MergeDeleted:
		_, err := tx.Exec("UPDATE vfile SET deleted=1 WHERE id=?", s.row.ID)
		return err
	case MergeMode:
		_, err := tx.Exec("UPDATE vfile SET isexe=? WHERE id=?", s.exe, s.row.ID)
		return err
	case MergeMerged, MergeConflict:
		// What a scan would make of content that now differs from the
		// baseline; a scan corrects it should the merge have changed nothing.
		next, err := nextChangedState(s.row.Chnged, scanFacts{hasBaseline: true}, alwaysDiffers)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE vfile SET mtime=0, chnged=?, isexe=? WHERE id=?",
			next, s.exe, s.row.ID); err != nil {
			return err
		}
		return insertVMerge(tx, s.row.ID, s.merged.rid, s.merged.uuid)
	case MergeRenamed:
		return nil // applyMergeRenames
	case MergeKept, MergeForked:
		// Fossil records a file its merge loop reached even when the merge
		// kept the checkout's copy (a binary file); a file no strategy saw
		// was not reached.
		if s.file.Strategy == "" || s.row.ID <= 0 || s.merged.rid <= 0 {
			return nil
		}
		return insertVMerge(tx, s.row.ID, s.merged.rid, s.merged.uuid)
	default:
		panic(fmt.Sprintf("checkout.applyMergeStep: unknown action %d", s.file.Action))
	}
}

// applyMergeRenames renames the files the merge renames, after every other
// step has written them under their old names, as fossil's merge does
// after its updates, merges and deletions.
func (c *Checkout) applyMergeRenames(tx *sql.Tx, plan mergePlan) error {
	var renamed []mergeStep
	for _, s := range plan.steps {
		if s.file.RenamedTo == "" {
			continue
		}
		if s.row.ID <= 0 || s.recordOnly {
			panic("checkout.applyMergeRenames: rename of a row not in vfile: " + s.file.Name)
		}
		renamed = append(renamed, s)
	}
	if len(renamed) == 0 {
		return nil
	}
	if err := c.moveRenamedFiles(renamed); err != nil {
		return err
	}
	// Two passes, so a name another rename vacates is free when taken. A
	// row still holding a new name, one removed here or deleted by the
	// merge, is renamed aside as fossil renames it.
	for _, s := range renamed {
		if _, err := tx.Exec(`UPDATE vfile SET origname=coalesce(origname,pathname),
			pathname=char(0)||id WHERE id=?`, s.row.ID); err != nil {
			return fmt.Errorf("rename %s: %w", s.file.Name, err)
		}
	}
	for _, s := range renamed {
		to := s.file.RenamedTo
		if _, err := tx.Exec(`UPDATE vfile SET origname=pathname,
			pathname=pathname||' (overwritten by rename)' WHERE vid=? AND pathname=?`,
			int64(plan.vid), to); err != nil {
			return fmt.Errorf("rename %s: %w", s.file.Name, err)
		}
		if _, err := tx.Exec("UPDATE vfile SET pathname=? WHERE id=?", to, s.row.ID); err != nil {
			return fmt.Errorf("rename %s: %w", s.file.Name, err)
		}
	}
	return nil
}

// moveRenamedFiles moves each renamed step's file to its new name. Every
// file is read before any is moved, so names swapped or chained between
// files move as fossil's temporary copies move them. A file missing from
// disk is not moved.
func (c *Checkout) moveRenamedFiles(renamed []mergeStep) error {
	type move struct {
		s        mergeStep
		data     []byte
		from, to string
		link     bool
	}
	disk, err := c.newDiskView()
	if err != nil {
		return err
	}
	moves := make([]move, 0, len(renamed))
	for _, s := range renamed {
		mv := move{s: s}
		if mv.from, err = c.safePath(s.file.Name); err != nil {
			return err
		}
		if mv.to, err = c.safePath(s.file.RenamedTo); err != nil {
			return err
		}
		info, err := disk.Stat(mv.from)
		if err != nil {
			return err
		}
		if !isFileOrLink(info) {
			continue
		}
		if mv.data, err = disk.Read(mv.from, info); err != nil {
			return err
		}
		mv.link = vfile.IsLink(info)
		moves = append(moves, mv)
	}
	for _, mv := range moves {
		if err := disk.Remove(mv.from); err != nil {
			return err
		}
	}
	for _, mv := range moves {
		exe := mv.s.exe
		if mv.s.file.Action == MergeUpdated {
			exe = mv.s.merged.exe
		}
		if err := disk.Write(mv.to, mv.data, exe, mv.link); err != nil {
			return err
		}
	}
	return nil
}

// applyMergeDiskStep is the filesystem half of a materialized merge step.
func (c *Checkout) applyMergeDiskStep(s mergeStep) error {
	switch s.file.Action {
	case MergeUpdated, MergeAdded:
		return c.writeMergedFile(s.file.Name, s.content, s.merged.exe)
	case MergeMerged, MergeConflict:
		return c.writeMergedFile(s.file.Name, s.content, s.exe)
	case MergeDeleted:
		full, err := c.safePath(s.file.Name)
		if err != nil {
			return err
		}
		return c.removeRegularFile(full)
	case MergeMode:
		return c.setMergedMode(s.file.Name, s.exe)
	default:
		panic("checkout.applyMergeDiskStep: step does not change disk")
	}
}

// writeMergedFile writes a merge's content for name with the executable
// bit exe.
func (c *Checkout) writeMergedFile(name string, data []byte, exe bool) error {
	if data == nil {
		panic("checkout.writeMergedFile: no content for " + name)
	}
	fullPath, err := c.safePath(name)
	if err != nil {
		return err
	}
	if err := c.env.Storage.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	perm := os.FileMode(0o644)
	if exe {
		perm = 0o755
	}
	if err := c.env.Storage.WriteFile(fullPath, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", fullPath, err)
	}
	// WriteFile keeps an existing file's mode.
	return c.setMergedMode(name, exe)
}

// setMergedMode gives name's file the executable bit exe, as fossil's
// file_setexe does (vfile.Disk.SetExe).
func (c *Checkout) setMergedMode(name string, exe bool) error {
	fullPath, err := c.safePath(name)
	if err != nil {
		return err
	}
	disk, err := c.newDiskView()
	if err != nil {
		return err
	}
	return disk.SetExe(fullPath, exe)
}

// insertVMerge records that merged-in artifact rid reached the checkout: id
// 0 for the merge as a whole (the next commit's merge parent), or the vfile
// id of a file merged from it. Fossil's checkout schema makes (id, mhash)
// unique; go-libfossil's never did, so the uniqueness is checked here.
func insertVMerge(tx *sql.Tx, id int64, rid libfossil.FslID, mhash string) error {
	if id < 0 {
		panic("checkout.insertVMerge: negative id")
	}
	if rid <= 0 {
		panic("checkout.insertVMerge: rid must be positive")
	}
	_, err := tx.Exec(`INSERT INTO vmerge(id, merge, mhash)
		SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM vmerge WHERE id=? AND mhash=?)`,
		id, int64(rid), mhash, id, mhash)
	if err != nil {
		return fmt.Errorf("record merge: %w", err)
	}
	return nil
}

// blobUUID returns the hash of artifact rid.
func blobUUID(q interface {
	QueryRow(string, ...any) *sql.Row
}, rid libfossil.FslID) (string, error) {
	var uuid string
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", int64(rid)).Scan(&uuid); err != nil {
		return "", fmt.Errorf("hash of rid %d: %w", rid, err)
	}
	return uuid, nil
}

// recordForks writes a conflict-table entry for each file the conflict-fork
// strategy kept, naming the ancestor, checkout and merged-in checkins.
func (c *Checkout) recordForks(plan mergePlan) error {
	for _, s := range plan.steps {
		if s.file.Action != MergeForked {
			continue
		}
		if err := merge.EnsureConflictTable(c.repo); err != nil {
			return fmt.Errorf("conflict table: %w", err)
		}
		var base, local, remote int64
		if s.fork.base {
			base = int64(plan.pivot)
		}
		if s.fork.local {
			local = int64(plan.vid)
		}
		if s.fork.remote {
			remote = int64(plan.mid)
		}
		err := merge.RecordConflictFork(c.repo, s.file.Name, base, local, remote)
		if err != nil {
			return fmt.Errorf("record fork for %s: %w", s.file.Name, err)
		}
	}
	return nil
}

// Conflicts lists the files, sorted, that are in conflict: changed files
// that still hold merge conflict markers, as fossil's changes command
// decides it. Resolving a conflict is editing the markers out of the file.
func (c *Checkout) Conflicts() ([]string, error) {
	if c == nil {
		panic("checkout.Conflicts: nil *Checkout")
	}
	vid, _, err := c.Version()
	if err != nil {
		return nil, fmt.Errorf("checkout.Conflicts: %w", err)
	}
	if err := c.ScanChanges(0); err != nil {
		return nil, fmt.Errorf("checkout.Conflicts: %w", err)
	}
	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil {
		return nil, fmt.Errorf("checkout.Conflicts: %w", err)
	}

	conflicted, err := c.unresolvedSparse()
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.IsRemoved() || r.IsLink > 0 || !r.ContentChanged() {
			continue
		}
		data, present, err := c.readWorkingFile(r)
		if err != nil {
			return nil, fmt.Errorf("checkout.Conflicts: %w", err)
		}
		if present && merge.HasConflictMarkers(data) {
			conflicted = append(conflicted, r.Pathname)
		}
	}
	sort.Strings(conflicted)
	return conflicted, nil
}
