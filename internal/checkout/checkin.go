package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/vfile"
)

// Enqueue adds files to the commit staging queue. If the queue is empty (nil),
// all changed files are implicitly enqueued. Once Enqueue is called, only
// explicitly enqueued files will be committed.
func (c *Checkout) Enqueue(opts EnqueueOpts) error {
	if c == nil {
		panic("checkout.Enqueue: nil *Checkout")
	}
	if c.checkinQueue == nil {
		c.checkinQueue = make(map[string]bool)
	}
	for _, p := range opts.Paths {
		c.checkinQueue[p] = true
		if opts.Callback != nil {
			if err := opts.Callback(p); err != nil {
				return fmt.Errorf("checkout.Enqueue: callback for %s: %w", p, err)
			}
		}
	}
	return nil
}

// Dequeue removes files from the commit staging queue. If opts.Paths is empty,
// clears the entire queue (restoring implicit all-files behavior).
// Returns error for API consistency / future-proofing.
func (c *Checkout) Dequeue(opts DequeueOpts) error {
	if c == nil {
		panic("checkout.Dequeue: nil *Checkout")
	}
	if len(opts.Paths) == 0 {
		c.checkinQueue = nil // dequeue all
		return nil
	}
	for _, p := range opts.Paths {
		delete(c.checkinQueue, p)
	}
	return nil
}

// IsEnqueued returns true if the named file will be included in the next commit.
// If the queue is nil (never initialized), all changed files are implicitly enqueued.
// If the queue exists but is empty (len == 0), nothing is enqueued.
// Returns error for API consistency / future-proofing.
func (c *Checkout) IsEnqueued(name string) (bool, error) {
	if c == nil {
		panic("checkout.IsEnqueued: nil *Checkout")
	}
	if c.checkinQueue == nil {
		return true, nil // nil queue = all changed files implicitly enqueued
	}
	return c.checkinQueue[name], nil
}

// DiscardQueue clears the commit staging queue, restoring implicit all-files behavior.
// Returns error for API consistency / future-proofing.
func (c *Checkout) DiscardQueue() error {
	if c == nil {
		panic("checkout.DiscardQueue: nil *Checkout")
	}
	c.checkinQueue = nil
	return nil
}

// vfileEntry holds a single vfile row for commit processing.
type vfileCommitEntry struct {
	pathname string
	origname string // prior pathname if renamed, else empty
	deleted  bool
	rid      int64
	isexe    bool
}

// collectVFileEntries reads vfile for all entries of the given version and
// sorts them into changed, deleted, and all-entries map. What a row means
// (an add, a removal, a rename, changed content) is vfile.Row's to say.
func (c *Checkout) collectVFileEntries(vid libfossil.FslID) (
	entries map[string]vfileCommitEntry,
	changedFiles []string,
	deletedFiles []string,
	err error,
) {
	rows, err := vfile.Load(c.db, int64(vid))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("checkout.Commit: %w", err)
	}

	entries = make(map[string]vfileCommitEntry, len(rows))
	for _, r := range rows {
		entries[r.Pathname] = vfileCommitEntry{
			pathname: r.Pathname,
			origname: r.RenamedFrom(),
			deleted:  r.IsRemoved(),
			rid:      r.RID,
			isexe:    r.IsExe > 0,
		}
		if r.IsRemoved() {
			deletedFiles = append(deletedFiles, r.Pathname)
		} else if r.ContentChanged() {
			changedFiles = append(changedFiles, r.Pathname)
		}
	}
	return entries, changedFiles, deletedFiles, nil
}

// buildCommitFiles constructs the complete file list for a new checkin by
// starting from the parent manifest, applying vfile changes (modified/deleted),
// and loading unchanged file content from the repo.
func (c *Checkout) buildCommitFiles(
	parentRID libfossil.FslID,
	vfEntries map[string]vfileCommitEntry,
	changedFiles []string,
	deletedFiles []string,
	shouldInclude func(string) bool,
) ([]manifest.File, error) {
	var parentFiles []manifest.FileEntry
	if parentRID != 0 {
		var err error
		parentFiles, err = manifest.ListFiles(c.repo, parentRID)
		if err != nil {
			return nil, fmt.Errorf("checkout.Commit: list parent files: %w", err)
		}
	}

	// Build a map of parent files (name → FileEntry for O(1) lookup)
	parentFileMap := make(map[string]manifest.FileEntry, len(parentFiles))
	for _, pf := range parentFiles {
		parentFileMap[pf.Name] = pf
	}

	// Start from parent file set
	fileMap := make(map[string]manifest.File, len(parentFiles))
	for _, pf := range parentFiles {
		fileMap[pf.Name] = manifest.File{
			Name: pf.Name,
			Perm: pf.Perm,
		}
	}

	// 1. Remove deleted files
	for _, name := range deletedFiles {
		if shouldInclude(name) {
			delete(fileMap, name)
		}
	}

	// 2. Update changed files with new content from disk
	loadedFromDisk := make(map[string]bool, len(changedFiles))
	for _, name := range changedFiles {
		if !shouldInclude(name) {
			continue
		}

		fullPath, err := c.safePath(name)
		if err != nil {
			return nil, fmt.Errorf("checkout.Commit: path traversal in %s: %w", name, err)
		}
		fileData, err := c.env.Storage.ReadFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("checkout.Commit: read %s: %w", name, err)
		}

		perm := ""
		if existing, ok := fileMap[name]; ok {
			perm = existing.Perm
		}

		// The executable marker is re-derived from the live on-disk mode by
		// restatCommitPerms below, for every included non-symlink file (not
		// only ones change scanning flagged) — so permission is decided
		// later, from the filesystem, rather than from vfile.isexe here.
		fileMap[name] = manifest.File{
			Name:    name,
			Content: fileData,
			Perm:    perm,
		}
		loadedFromDisk[name] = true
	}

	// 2b. Apply renames: retire each renamed file's old pathname and record
	// the prior name on the surviving new-name entry, so the file is emitted
	// exactly once as a rename F-card rather than twice.
	c.applyRenames(fileMap, vfEntries, shouldInclude)

	// 3. For unchanged files, load content from the repo.
	//
	// loadedFromDisk (not len(entry.Content) > 0) is the sentinel for
	// "already handled in step 2" — a legitimately empty file's Content is
	// indistinguishable from "not yet loaded" by length alone, and using
	// length here mistook a newly-added zero-length file for one still
	// needing a blob lookup it doesn't have (issue #68).
	for name, entry := range fileMap {
		if loadedFromDisk[name] {
			continue
		}

		// A tracked file this commit is scoped to (shouldInclude) but that
		// is missing from disk cannot be silently carried forward with its
		// stale content — that would record a commit claiming content that
		// no longer exists (issue #79). A file outside this commit's scope
		// (relevant only for an explicit Enqueue) is left untouched exactly
		// as before: canonical Fossil only fails on a missing file that is
		// actually part of the commit being made.
		//
		// Only files with a live vfile row are checked — matching
		// restatCommitPerms and canonical Fossil's own vfile-driven manifest
		// loop. A name present only in the parent manifest (no vfEntries
		// row) isn't tracked by this checkout's vfile at all; there is
		// nothing to have "gone missing" from a local scan that never
		// covered it.
		if _, hasVFileRow := vfEntries[name]; hasVFileRow && shouldInclude(name) {
			fullPath, err := c.safePath(name)
			if err != nil {
				return nil, fmt.Errorf("checkout.Commit: path traversal in %s: %w", name, err)
			}
			if _, err := c.env.Storage.Stat(fullPath); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf(
						"checkout.Commit: tracked file missing from disk: %s", name,
					)
				}
				return nil, fmt.Errorf("checkout.Commit: stat %s: %w", name, err)
			}
		}

		var fileRID int64
		if ve, ok := vfEntries[name]; ok {
			fileRID = ve.rid
		} else if pf, ok := parentFileMap[name]; ok {
			// File is in parent but not in vfile — look up RID by UUID
			var rid int64
			err := c.repo.DB().QueryRow("SELECT rid FROM blob WHERE uuid = ?", pf.UUID).Scan(&rid)
			if err != nil {
				return nil, fmt.Errorf("checkout.Commit: resolve RID for %s: %w", name, err)
			}
			fileRID = rid
		}

		if fileRID == 0 {
			return nil, fmt.Errorf("checkout.Commit: no RID for unchanged file %s", name)
		}

		fileData, err := content.Expand(c.repo.DB(), libfossil.FslID(fileRID))
		if err != nil {
			return nil, fmt.Errorf("checkout.Commit: expand %s: %w", name, err)
		}

		fileMap[name] = manifest.File{
			Name:    name,
			Content: fileData,
			Perm:    entry.Perm,
			OldName: entry.OldName,
		}
	}

	// 4. Independently re-stat every included file's on-disk executable bit,
	// belt-and-braces, regardless of whether change scanning flagged it.
	if err := c.restatCommitPerms(fileMap, vfEntries, shouldInclude); err != nil {
		return nil, err
	}

	// Convert to slice
	commitFiles := make([]manifest.File, 0, len(fileMap))
	for _, f := range fileMap {
		commitFiles = append(commitFiles, f)
	}
	return commitFiles, nil
}

// applyRenames rewrites fileMap so each renamed file is emitted exactly once,
// under its new name, carrying its prior name for the rename F-card.
//
// It is driven off vfile.origname rather than the changed-files list because a
// pure rename (identical content) is not flagged as changed by ScanChanges —
// so without this pass the new name would never enter fileMap and the old name
// would carry forward from the parent manifest, double-emitting the content
// (the #51 defect). This mirrors canonical Fossil, whose manifest query
// selects rows where pathname!=origname independent of the chnged flag
// (src/checkin.c:1939-1944).
//
// The retired old name is deleted from fileMap unless it has independently
// reappeared as its own live vfile pathname (a rename that reuses a freed
// name), in which case that entry is left for the normal passes to handle.
//
// A renamed row that is also marked deleted retires both names and emits
// nothing, matching canonical Fossil's exclusion of deleted rows from the
// manifest query.
func (c *Checkout) applyRenames(
	fileMap map[string]manifest.File,
	vfEntries map[string]vfileCommitEntry,
	shouldInclude func(string) bool,
) {
	if fileMap == nil {
		panic("checkout.applyRenames: fileMap must not be nil")
	}
	if shouldInclude == nil {
		panic("checkout.applyRenames: shouldInclude must not be nil")
	}

	for name, ve := range vfEntries {
		if ve.origname == "" || ve.origname == name {
			continue
		}
		if !shouldInclude(name) {
			continue
		}

		// A renamed file that was subsequently deleted must not be emitted at
		// all — under either name. Canonical Fossil's manifest query excludes
		// such rows outright (WHERE NOT deleted OR NOT is_selected), and the
		// deleted-files pass above cannot retire the old name, since it only
		// knows the file's current pathname.
		if ve.deleted {
			if _, reAdded := vfEntries[ve.origname]; !reAdded {
				delete(fileMap, ve.origname)
			}
			delete(fileMap, name)
			continue
		}

		// Ensure a new-name entry exists so the file is emitted under its new
		// name. A content-changed rename already has one (from the changed-file
		// pass); a pure rename seeds an empty entry whose content the
		// unchanged-file pass fills from the vfile blob.
		entry, ok := fileMap[name]
		if !ok {
			entry = manifest.File{Name: name, Perm: fileMap[ve.origname].Perm}
		}
		entry.OldName = ve.origname
		fileMap[name] = entry

		if _, reAdded := vfEntries[ve.origname]; !reAdded {
			delete(fileMap, ve.origname)
		}
	}
}

// restatCommitPerms re-derives each included file's executable permission
// directly from its live on-disk mode, overriding whatever change scanning
// captured earlier. This exists because change scanning compares content
// hashes: a file whose only edit is a chmod has an unchanged hash, so
// vfile.chnged/isexe can go stale and silently carry forward the wrong
// permission. Manifest generation must consult the filesystem itself for
// every file it is about to serialize — mirroring canonical Fossil's
// belt-and-braces re-stat in src/checkin.c — rather than trust a cached
// flag a second time.
//
// Only files that actually have a vfile row are considered — matching
// canonical Fossil, whose manifest loop is driven by a query over vfile
// (JOIN blob) rather than the union of parent-manifest and vfile names.
// Files carried forward from the parent manifest but no longer present in
// the local vfile (should not happen once LoadVFile has run, but is not an
// invariant this function enforces) are left untouched, since there is no
// live vfile row to justify a re-stat. Symlinks (Perm == "l") and files not
// selected for this commit are also left untouched. On Windows,
// os.FileMode never carries an owner-execute bit, so modeIsExecutable
// always returns false and this is a no-op.
//
// A file that has gone missing from disk (deleted outside of Unmanage, or
// otherwise untracked-but-not-told) is skipped, carrying its existing Perm
// forward unchanged. This function's job is fixing missed mode changes, not
// detecting stale tracking, so it stays lenient here — but that leniency is
// safe because buildCommitFiles already rejected any in-scope missing file
// before this function ever runs (issue #79); a missing file only reaches
// here at all when it was out of scope (shouldInclude false), in which case
// carrying its Perm forward unchanged is correct. Any other Stat error
// (permission denied, I/O fault, ...) is a real problem and still fails
// the commit.
func (c *Checkout) restatCommitPerms(
	fileMap map[string]manifest.File,
	vfEntries map[string]vfileCommitEntry,
	shouldInclude func(string) bool,
) error {
	if fileMap == nil {
		panic("checkout.restatCommitPerms: fileMap must not be nil")
	}
	if shouldInclude == nil {
		panic("checkout.restatCommitPerms: shouldInclude must not be nil")
	}

	for name := range vfEntries {
		entry, ok := fileMap[name]
		if !ok {
			continue // deleted, or otherwise dropped from this commit
		}
		if entry.Perm == "l" {
			continue
		}
		if !shouldInclude(name) {
			continue
		}

		fullPath, err := c.safePath(name)
		if err != nil {
			return fmt.Errorf("checkout.Commit: path traversal in %s: %w", name, err)
		}
		info, err := c.env.Storage.Stat(fullPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // missing on disk: carry the existing Perm forward unchanged
			}
			return fmt.Errorf("checkout.Commit: stat %s: %w", name, err)
		}

		if modeIsExecutable(info.Mode()) {
			entry.Perm = "x"
		} else {
			entry.Perm = ""
		}
		if entry.Perm != "" && entry.Perm != "x" {
			panic("checkout.restatCommitPerms: re-derived perm must be \"\" or \"x\"")
		}
		fileMap[name] = entry
	}

	return nil
}

// ErrNothingToCommit refuses a commit that would record nothing: no file
// changed and no merge pending, or no file at all. CommitOpts.AllowEmpty
// commits an unchanged checkout anyway.
var ErrNothingToCommit = errors.New("checkout.Commit: nothing to commit")

// ErrUnresolvedConflicts refuses a commit of files that still hold merge
// conflict markers. CommitOpts.AllowConflict commits them anyway.
var ErrUnresolvedConflicts = errors.New("checkout.Commit: unresolved merge conflicts")

// errPartialMergeCommit refuses committing only some files while a merge is
// pending: the merge parent would be recorded over a half-merged tree, which
// fossil's commit refuses too.
var errPartialMergeCommit = errors.New("checkout.Commit: cannot commit some files of a merge")

// commitTagCards builds the T-cards for opts.Branch and opts.Tags.
func commitTagCards(opts CommitOpts) []deck.TagCard {
	var tagCards []deck.TagCard
	if opts.Branch != "" {
		tagCards = append(tagCards, deck.TagCard{
			Type: deck.TagPropagating, Name: "branch",
			UUID: "*", Value: opts.Branch,
		})
		tagCards = append(tagCards, deck.TagCard{
			Type: deck.TagSingleton,
			Name: "sym-" + opts.Branch,
			UUID: "*",
		})
	}
	for _, t := range opts.Tags {
		tagCards = append(tagCards, deck.TagCard{
			Type: deck.TagSingleton, Name: t, UUID: "*",
		})
	}
	return tagCards
}

// mergeParents lists the checkins pending merges bring in, which the next
// commit records as merge parents: whole-checkout merges (vmerge id 0) and
// integrates (id -4), as fossil's commit reads them. Per-file rows (id > 0)
// and cherrypicks or backouts (-1, -2) are not parents.
func (c *Checkout) mergeParents(parent libfossil.FslID) (parents []libfossil.FslID, err error) {
	rows, err := c.db.Query(
		"SELECT DISTINCT merge FROM vmerge WHERE (id=0 OR id<-2) AND merge<>? ORDER BY merge",
		int64(parent),
	)
	if err != nil {
		return nil, fmt.Errorf("checkout.Commit: query vmerge: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("checkout.Commit: close vmerge query: %w", cerr)
		}
	}()

	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return nil, fmt.Errorf("checkout.Commit: scan vmerge: %w", err)
		}
		if rid <= 0 {
			return nil, fmt.Errorf("checkout.Commit: vmerge names invalid rid %d", rid)
		}
		parents = append(parents, libfossil.FslID(rid))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkout.Commit: read vmerge: %w", err)
	}
	return parents, nil
}

// finalizeCommit updates vvar, reloads vfile, and clears the checkin queue
// after a successful manifest.Checkin.
func (c *Checkout) finalizeCommit(newRID libfossil.FslID, newUUID string) error {
	if err := setVVar(c.db, "checkout", strconv.FormatInt(int64(newRID), 10)); err != nil {
		return fmt.Errorf("checkout.Commit: set checkout vvar: %w", err)
	}
	if err := setVVar(c.db, "checkout-hash", newUUID); err != nil {
		return fmt.Errorf("checkout.Commit: set checkout-hash vvar: %w", err)
	}
	if err := c.LoadVFile(newRID, true); err != nil {
		return fmt.Errorf("checkout.Commit: reload vfile: %w", err)
	}
	// The merges are part of the new checkin now.
	if _, err := c.db.Exec("DELETE FROM vmerge"); err != nil {
		return fmt.Errorf("checkout.Commit: clear vmerge: %w", err)
	}
	c.checkinQueue = nil
	return nil
}

// Commit creates a new checkin from staged files in the checkout.
// Returns the new manifest RID and UUID.
func (c *Checkout) Commit(opts CommitOpts) (libfossil.FslID, string, error) {
	if c == nil {
		panic("checkout.Commit: nil *Checkout")
	}

	in, err := c.gatherCommit(opts)
	if err != nil {
		return 0, "", err
	}

	ctx := c.obs.CommitStarted(context.Background(), CommitStart{
		FilesEnqueued: in.enqueued,
		Branch:        opts.Branch,
		User:          opts.User,
	})

	var result CommitEnd
	defer func() { c.obs.CommitCompleted(ctx, result) }()

	commitFiles, err := c.buildCommitFiles(
		in.parent, in.entries, in.changed,
		in.deleted, in.include,
	)
	if err != nil {
		result.Err = err
		return 0, "", err
	}
	if len(commitFiles) == 0 {
		result.Err = ErrNothingToCommit
		return 0, "", result.Err
	}

	newRID, newUUID, err := manifest.Checkin(c.repo, c.checkinOptions(opts, in, commitFiles))
	if err != nil {
		result.Err = fmt.Errorf("checkout.Commit: checkin: %w", err)
		return 0, "", result.Err
	}

	if err := c.finalizeCommit(newRID, newUUID); err != nil {
		result.Err = err
		return 0, "", err
	}

	result.RID = newRID
	result.UUID = newUUID
	result.FilesCommit = len(commitFiles)
	return newRID, newUUID, nil
}

// commitInputs is what Commit learns from the checkout before it builds the
// checkin.
type commitInputs struct {
	parent       libfossil.FslID
	entries      map[string]vfileCommitEntry
	changed      []string
	deleted      []string
	mergeParents []libfossil.FslID
	include      func(name string) bool // the checkin queue's filter
	enqueued     int
}

// gatherCommit scans the checkout, runs the pre-commit check and collects
// the files, merge parents and queue filter the commit works from.
func (c *Checkout) gatherCommit(opts CommitOpts) (commitInputs, error) {
	var in commitInputs
	var err error
	if in.parent, _, err = c.Version(); err != nil {
		return in, fmt.Errorf("checkout.Commit: %w", err)
	}
	if err := c.ScanChanges(ScanHash); err != nil {
		return in, fmt.Errorf("checkout.Commit: scan: %w", err)
	}
	if opts.PreCommitCheck != nil {
		if err := opts.PreCommitCheck(); err != nil {
			return in, fmt.Errorf("checkout.Commit: pre-commit check: %w", err)
		}
	}

	in.entries, in.changed, in.deleted, err = c.collectVFileEntries(in.parent)
	if err != nil {
		return in, err
	}
	if in.mergeParents, err = c.mergeParents(in.parent); err != nil {
		return in, err
	}

	selected, err := c.selectedFiles(in.entries)
	if err != nil {
		return in, err
	}
	queueActive := selected != nil
	if queueActive && len(in.mergeParents) > 0 {
		return in, errPartialMergeCommit
	}
	in.include = func(name string) bool {
		if !queueActive {
			return true
		}
		return selected[name]
	}
	in.enqueued = len(in.changed)
	if queueActive {
		in.enqueued = len(selected)
	}
	return in, c.checkCommittable(opts, in, queueActive)
}

// selectedFiles resolves the checkin queue the way fossil's commit resolves
// the files it is given: a name selects the tracked file of that name, or
// every tracked file under it when it names a directory, and "." selects
// everything. It returns nil when every file is selected, and refuses a name
// that selects nothing, as fossil does ("knows nothing about").
func (c *Checkout) selectedFiles(entries map[string]vfileCommitEntry) (map[string]bool, error) {
	if len(c.checkinQueue) == 0 {
		return nil, nil
	}
	selected := make(map[string]bool, len(c.checkinQueue))
	var unknown []string
	for queued := range c.checkinQueue {
		name := path.Clean(filepath.ToSlash(queued))
		if name == "." {
			return nil, nil
		}
		found := false
		for tracked := range entries {
			if tracked == name || strings.HasPrefix(tracked, name+"/") {
				selected[tracked] = true
				found = true
			}
		}
		if !found {
			unknown = append(unknown, queued)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("checkout.Commit: not tracked: %s", strings.Join(unknown, ", "))
	}
	if len(selected) == 0 {
		panic("checkout.selectedFiles: names resolved to no files")
	}
	return selected, nil
}

// checkCommittable refuses, as fossil's commit does, a commit with nothing
// to record (unless opts.AllowEmpty) and one that includes a file still
// holding conflict markers (unless opts.AllowConflict). A pending merge is
// something to record even when no file changed.
func (c *Checkout) checkCommittable(opts CommitOpts, in commitInputs, queueActive bool) error {
	if !opts.AllowEmpty && len(in.mergeParents) == 0 {
		changed, err := c.hasSelectedChange(in)
		if err != nil {
			return err
		}
		if !changed && queueActive {
			return fmt.Errorf("%w: none of the selected files have changed", ErrNothingToCommit)
		}
		if !changed {
			return fmt.Errorf("%w: nothing has changed", ErrNothingToCommit)
		}
	}
	if opts.AllowConflict {
		return nil
	}
	return c.checkNoConflicts(in)
}

// hasModeDrift reports whether a selected file's executable bit on disk
// differs from the checkout's record: a chmod the scan does not record,
// which the commit picks up (see restatCommitPerms).
func (c *Checkout) hasModeDrift(in commitInputs) (bool, error) {
	for name, e := range in.entries {
		if e.deleted || !in.include(name) {
			continue
		}
		fullPath, err := c.safePath(name)
		if err != nil {
			return false, fmt.Errorf("checkout.Commit: %w", err)
		}
		info, err := c.env.Storage.Stat(fullPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checkout.Commit: stat %s: %w", fullPath, err)
		}
		if info.Mode().IsRegular() && modeIsExecutable(info.Mode()) != e.isexe {
			return true, nil
		}
	}
	return false, nil
}

// hasSelectedChange reports whether a selected file is changed, added,
// removed or renamed, or has had its executable bit changed on disk.
func (c *Checkout) hasSelectedChange(in commitInputs) (bool, error) {
	for _, name := range in.changed {
		if in.include(name) {
			return true, nil
		}
	}
	for name, e := range in.entries {
		if !in.include(name) {
			continue
		}
		if e.deleted || e.origname != "" {
			return true, nil
		}
	}
	return c.hasModeDrift(in)
}

// checkNoConflicts refuses a commit that includes a file still holding merge
// conflict markers, as fossil's commit does unless --allow-conflict is given.
func (c *Checkout) checkNoConflicts(in commitInputs) error {
	if in.include == nil {
		panic("checkout.checkNoConflicts: no include filter")
	}
	var conflicted []string
	for _, name := range in.changed {
		if !in.include(name) {
			continue
		}
		e, ok := in.entries[name]
		if !ok {
			panic("checkout.checkNoConflicts: changed file without an entry: " + name)
		}
		data, present, err := c.readWorkingFile(vfile.Row{Pathname: e.pathname})
		if err != nil {
			return fmt.Errorf("checkout.Commit: %w", err)
		}
		if present && merge.HasConflictMarkers(data) {
			conflicted = append(conflicted, name)
		}
	}
	if len(conflicted) > 0 {
		return fmt.Errorf("%w: %s", ErrUnresolvedConflicts, strings.Join(conflicted, ", "))
	}
	return nil
}

// checkinOptions assembles the manifest for the commit.
func (c *Checkout) checkinOptions(
	opts CommitOpts, in commitInputs, files []manifest.File,
) manifest.CheckinOpts {
	commitTime := opts.Time
	if commitTime.IsZero() {
		commitTime = c.env.Clock.Now()
	}
	checkinOpts := manifest.CheckinOpts{
		Files:   files,
		Comment: opts.Message,
		User:    opts.User,
		Parent:  in.parent,
		Time:    commitTime,
		Delta:   opts.Delta,
	}
	if len(in.mergeParents) > 0 {
		checkinOpts.MergeParents = in.mergeParents
	}
	if tagCards := commitTagCards(opts); len(tagCards) > 0 {
		checkinOpts.Tags = tagCards
	}
	return checkinOpts
}
