package checkout

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/danmestas/libfossil/internal/content"
	"github.com/danmestas/libfossil/internal/deck"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/manifest"
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
	changed  bool
	deleted  bool
	rid      int64
}

// collectVFileEntries queries vfile for all entries of the given version and
// classifies them into changed, deleted, and all-entries map.
func (c *Checkout) collectVFileEntries(vid libfossil.FslID) (
	entries map[string]vfileCommitEntry,
	changedFiles []string,
	deletedFiles []string,
	err error,
) {
	rows, err := c.db.Query(`
		SELECT pathname, CAST(chnged AS INTEGER), CAST(deleted AS INTEGER), rid
		FROM vfile
		WHERE vid = ?
	`, int64(vid))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("checkout.Commit: query vfile: %w", err)
	}
	defer rows.Close()

	entries = make(map[string]vfileCommitEntry)

	for rows.Next() {
		var pathname string
		var chnged, deleted int
		var rid sql.NullInt64
		if err := rows.Scan(&pathname, &chnged, &deleted, &rid); err != nil {
			return nil, nil, nil, fmt.Errorf("checkout.Commit: scan vfile: %w", err)
		}
		entries[pathname] = vfileCommitEntry{
			pathname: pathname,
			changed:  chnged > 0,
			deleted:  deleted > 0,
			rid:      rid.Int64,
		}

		if deleted > 0 {
			deletedFiles = append(deletedFiles, pathname)
		} else if chnged > 0 || rid.Int64 == 0 {
			// rid=0 means newly added (never committed) — always treat
			// as changed even if ScanChanges reset chnged to 0.
			changedFiles = append(changedFiles, pathname)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("checkout.Commit: vfile rows: %w", err)
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

		fileMap[name] = manifest.File{
			Name:    name,
			Content: fileData,
			Perm:    perm,
		}
	}

	// 3. For unchanged files, load content from the repo
	for name, entry := range fileMap {
		if len(entry.Content) > 0 {
			continue // already have content (was changed)
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
		}
	}

	// Convert to slice
	commitFiles := make([]manifest.File, 0, len(fileMap))
	for _, f := range fileMap {
		commitFiles = append(commitFiles, f)
	}
	return commitFiles, nil
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
	if _, err := c.LoadVFile(newRID, true); err != nil {
		return fmt.Errorf("checkout.Commit: reload vfile: %w", err)
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

	parentRID, _, err := c.Version()
	if err != nil {
		return 0, "", fmt.Errorf("checkout.Commit: %w", err)
	}

	if err := c.ScanChanges(ScanHash); err != nil {
		return 0, "", fmt.Errorf("checkout.Commit: scan: %w", err)
	}

	if opts.PreCommitCheck != nil {
		if err := opts.PreCommitCheck(); err != nil {
			return 0, "", fmt.Errorf("checkout.Commit: pre-commit check: %w", err)
		}
	}

	vfEntries, changedFiles, deletedFiles, err := c.collectVFileEntries(parentRID)
	if err != nil {
		return 0, "", err
	}

	queueActive := c.checkinQueue != nil && len(c.checkinQueue) > 0
	shouldInclude := func(name string) bool {
		if !queueActive {
			return true
		}
		return c.checkinQueue[name]
	}

	enqueuedCount := len(changedFiles)
	if queueActive {
		enqueuedCount = len(c.checkinQueue)
	}

	ctx := c.obs.CommitStarted(context.Background(), CommitStart{
		FilesEnqueued: enqueuedCount,
		Branch:        opts.Branch,
		User:          opts.User,
	})

	var result CommitEnd
	defer func() { c.obs.CommitCompleted(ctx, result) }()

	commitFiles, err := c.buildCommitFiles(
		parentRID, vfEntries, changedFiles,
		deletedFiles, shouldInclude,
	)
	if err != nil {
		result.Err = err
		return 0, "", err
	}

	commitTime := opts.Time
	if commitTime.IsZero() {
		commitTime = c.env.Clock.Now()
	}

	// Build T-cards from CommitOpts.Branch and Tags.
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

	checkinOpts := manifest.CheckinOpts{
		Files:   commitFiles,
		Comment: opts.Message,
		User:    opts.User,
		Parent:  parentRID,
		Time:    commitTime,
		Delta:   opts.Delta,
	}
	if len(tagCards) > 0 {
		checkinOpts.Tags = tagCards
	}

	newRID, newUUID, err := manifest.Checkin(c.repo, checkinOpts)
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
