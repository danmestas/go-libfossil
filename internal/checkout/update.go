package checkout

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/merge"
)

// CalcUpdateVersion finds the latest leaf checkin to update to.
// Returns 0 if the checkout is already at the latest version.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) CalcUpdateVersion() (libfossil.FslID, error) {
	if c == nil {
		panic("checkout.CalcUpdateVersion: nil *Checkout")
	}

	currentRID, _, err := c.Version()
	if err != nil {
		return 0, fmt.Errorf("checkout.CalcUpdateVersion: %w", err)
	}

	var tipRID int64
	err = c.repo.DB().QueryRow(`
		SELECT l.rid FROM leaf l
		JOIN event e ON e.objid=l.rid
		WHERE e.type='ci'
		ORDER BY e.mtime DESC LIMIT 1
	`).Scan(&tipRID)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil // no checkins at all
		}
		return 0, fmt.Errorf("checkout.CalcUpdateVersion: %w", err)
	}

	if libfossil.FslID(tipRID) == currentRID {
		return 0, nil // already at latest
	}

	return libfossil.FslID(tipRID), nil
}

// updateFileMaps holds name→UUID maps for the current, target, and ancestor versions.
type updateFileMaps struct {
	current  map[string]string
	target   map[string]string
	ancestor map[string]string
	allNames map[string]bool
}

// buildFileMaps builds name→UUID maps for the current, target, and ancestor
// file lists, plus the union of all file names.
func buildFileMaps(currentFiles, targetFiles, ancestorFiles []manifest.FileEntry) updateFileMaps {
	m := updateFileMaps{
		current:  make(map[string]string, len(currentFiles)),
		target:   make(map[string]string, len(targetFiles)),
		ancestor: make(map[string]string, len(ancestorFiles)),
		allNames: make(map[string]bool, len(currentFiles)+len(targetFiles)),
	}
	for _, f := range currentFiles {
		m.current[f.Name] = f.UUID
		m.allNames[f.Name] = true
	}
	for _, f := range targetFiles {
		m.target[f.Name] = f.UUID
		m.allNames[f.Name] = true
	}
	for _, f := range ancestorFiles {
		m.ancestor[f.Name] = f.UUID
	}
	return m
}

// processFileUpdates iterates over all file names and applies update/merge
// logic for each one. Returns the paths actually written, removed, and
// conflicted — not merely counts — so the caller can report exactly which
// files a consumer needs to inspect.
//
// Bounded: the loop and the three returned slices are each bounded by
// len(maps.allNames), the union of the current and target manifests for
// this update — finite for any real checkin.
func (c *Checkout) processFileUpdates(
	ctx context.Context,
	maps updateFileMaps,
	strategy merge.Strategy,
	opts UpdateOpts,
	checkinTime time.Time,
) (filesWritten, filesRemoved, conflicted []string, err error) {
	for name := range maps.allNames {
		curUUID, inCurrent := maps.current[name]
		tgtUUID, inTarget := maps.target[name]
		ancUUID := maps.ancestor[name]

		change, ferr := c.updateFile(
			name, curUUID, tgtUUID, ancUUID,
			inCurrent, inTarget, strategy, opts.DryRun,
		)
		if ferr != nil {
			return filesWritten, filesRemoved, conflicted, fmt.Errorf("checkout.Update: %w", ferr)
		}

		if change == UpdateNone {
			continue
		}

		// Apply checkin mtime to written files.
		if opts.SetMTime && !opts.DryRun && !checkinTime.IsZero() && change != UpdateRemoved {
			fullPath, _ := c.safePath(name)
			if fullPath != "" {
				_ = c.env.Storage.Chtimes(fullPath, checkinTime, checkinTime)
			}
		}

		switch change {
		case UpdateAdded, UpdateUpdated, UpdateMerged:
			filesWritten = append(filesWritten, name)
		case UpdateConflictMerged:
			filesWritten = append(filesWritten, name)
			conflicted = append(conflicted, name)
			c.obs.Error(ctx, fmt.Errorf(
				"checkout.Update: merge conflict in %s", name,
			))
		case UpdateRemoved:
			filesRemoved = append(filesRemoved, name)
		}

		c.obs.ExtractFileCompleted(ctx, name, change)

		if opts.Callback != nil {
			if cerr := opts.Callback(name, change); cerr != nil {
				return filesWritten, filesRemoved, conflicted, fmt.Errorf(
					"checkout.Update: callback for %s: %w", name, cerr,
				)
			}
		}
	}
	return filesWritten, filesRemoved, conflicted, nil
}

// buildUpdateMaps gathers file lists for current, target, and their common
// ancestor, then builds the combined name→UUID maps for 3-way comparison.
func (c *Checkout) buildUpdateMaps(currentRID, target libfossil.FslID) (updateFileMaps, error) {
	currentFiles, err := manifest.ListFiles(c.repo, currentRID)
	if err != nil {
		return updateFileMaps{}, fmt.Errorf("checkout.Update: list current files: %w", err)
	}
	targetFiles, err := manifest.ListFiles(c.repo, target)
	if err != nil {
		return updateFileMaps{}, fmt.Errorf("checkout.Update: list target files: %w", err)
	}

	ancestor, err := merge.FindCommonAncestor(c.repo, currentRID, target)
	if err != nil {
		ancestor = currentRID
	}

	var ancestorFiles []manifest.FileEntry
	if ancestor == currentRID {
		ancestorFiles = currentFiles
	} else {
		ancestorFiles, err = manifest.ListFiles(c.repo, ancestor)
		if err != nil {
			return updateFileMaps{}, fmt.Errorf("checkout.Update: list ancestor files: %w", err)
		}
	}

	return buildFileMaps(currentFiles, targetFiles, ancestorFiles), nil
}

// Update updates the checkout to a new version, performing 3-way merge where
// needed to preserve local modifications.
//
// If opts.TargetRID is 0, CalcUpdateVersion is used to find the target.
// If the target equals the current version, Update returns a zero-value
// UpdateResult and a nil error (nothing to do).
//
// A non-nil error means the update genuinely failed; the returned
// UpdateResult's contents are not meaningful in that case (see UpdateResult
// for the outcome mapping). Update never returns a non-nil error together
// with a non-empty UpdateResult.Conflicted — a conflicted merge is a
// success, not a failure.
//
// Panics if c is nil (TigerStyle precondition).
func (c *Checkout) Update(opts UpdateOpts) (UpdateResult, error) {
	if c == nil {
		panic("checkout.Update: nil *Checkout")
	}

	// Determine target
	target := opts.TargetRID
	if target == 0 {
		var err error
		target, err = c.CalcUpdateVersion()
		if err != nil {
			return UpdateResult{}, fmt.Errorf("checkout.Update: %w", err)
		}
		if target == 0 {
			return UpdateResult{}, nil // nothing to update
		}
	}

	// Get current version
	currentRID, _, err := c.Version()
	if err != nil {
		return UpdateResult{}, fmt.Errorf("checkout.Update: %w", err)
	}
	if currentRID == target {
		return UpdateResult{}, nil // already at target
	}

	// Start observer
	ctx := c.obs.ExtractStarted(context.Background(), ExtractStart{
		Operation: "update",
		TargetRID: target,
	})

	var filesWritten, filesRemoved, conflicted []string
	var updateErr error

	defer func() {
		c.obs.ExtractCompleted(ctx, ExtractEnd{
			Operation:    "update",
			TargetRID:    target,
			FilesWritten: len(filesWritten),
			FilesRemoved: len(filesRemoved),
			Conflicts:    len(conflicted),
			Err:          updateErr,
		})
	}()

	// Build 3-version file maps
	maps, err := c.buildUpdateMaps(currentRID, target)
	if err != nil {
		updateErr = err
		return UpdateResult{}, updateErr
	}

	strategy, ok := merge.StrategyByName("three-way")
	if !ok {
		updateErr = fmt.Errorf("checkout.Update: three-way merge strategy not registered")
		return UpdateResult{}, updateErr
	}

	// Look up checkin timestamp for SetMTime.
	var checkinTime time.Time
	if opts.SetMTime && !opts.DryRun {
		var mtimeRaw any
		if err := c.repo.DB().QueryRow(
			"SELECT mtime FROM event WHERE objid = ? AND type = 'ci'",
			int64(target),
		).Scan(&mtimeRaw); err == nil {
			checkinTime, _ = db.ScanTime(mtimeRaw)
		}
	}

	// Process each file
	filesWritten, filesRemoved, conflicted, updateErr = c.processFileUpdates(ctx, maps, strategy, opts, checkinTime)
	if updateErr != nil {
		return UpdateResult{}, updateErr
	}

	// Finalize: reload vfile and update vvar
	if updateErr = c.finalizeUpdate(target); updateErr != nil {
		return UpdateResult{}, updateErr
	}

	// maps.allNames is a map, so processFileUpdates' iteration order (and
	// hence the order these slices were built in) is randomized by the Go
	// runtime. Sort so the result is deterministic: golden-file and
	// reflect.DeepEqual assertions against these paths are only meaningful
	// if the same input produces the same order every run.
	sort.Strings(filesWritten)
	sort.Strings(filesRemoved)
	sort.Strings(conflicted)

	return UpdateResult{
		FilesWritten: filesWritten,
		FilesRemoved: filesRemoved,
		Conflicted:   conflicted,
	}, nil
}

// finalizeUpdate reloads vfile for the target version, looks up its UUID,
// and updates the vvar checkout/checkout-hash entries.
func (c *Checkout) finalizeUpdate(target libfossil.FslID) error {
	if _, err := c.LoadVFile(target, true); err != nil {
		return fmt.Errorf("checkout.Update: reload vfile: %w", err)
	}

	var targetUUID string
	err := c.repo.DB().QueryRow("SELECT uuid FROM blob WHERE rid = ?", int64(target)).Scan(&targetUUID)
	if err != nil {
		return fmt.Errorf("checkout.Update: query target uuid: %w", err)
	}

	if err := setVVar(c.db, "checkout", strconv.FormatInt(int64(target), 10)); err != nil {
		return fmt.Errorf("checkout.Update: %w", err)
	}
	if err := setVVar(c.db, "checkout-hash", targetUUID); err != nil {
		return fmt.Errorf("checkout.Update: %w", err)
	}
	return nil
}

// updateFile handles the update logic for a single file across three versions.
// Returns the UpdateChange type applied, or UpdateNone if the file was skipped.
func (c *Checkout) updateFile(
	name string,
	curUUID, tgtUUID, ancUUID string,
	inCurrent, inTarget bool,
	strategy merge.Strategy,
	dryRun bool,
) (UpdateChange, error) {
	switch {
	case !inCurrent && inTarget:
		// New file in target — extract and write
		if !dryRun {
			if err := c.writeFileFromUUID(name, tgtUUID); err != nil {
				return UpdateNone, fmt.Errorf("add %s: %w", name, err)
			}
		}
		return UpdateAdded, nil

	case inCurrent && !inTarget:
		// File removed in target — delete from disk
		if !dryRun {
			fullPath, err := c.safePath(name)
			if err != nil {
				return UpdateNone, fmt.Errorf("remove %s: %w", name, err)
			}
			if err := c.env.Storage.Remove(fullPath); err != nil {
				// Ignore errors if file already missing
				if !os.IsNotExist(err) {
					return UpdateNone, fmt.Errorf("remove %s: %w", name, err)
				}
			}
		}
		return UpdateRemoved, nil

	case inCurrent && inTarget:
		if curUUID == tgtUUID {
			// Same content — skip
			return UpdateNone, nil
		}

		// Different content — determine who changed
		if curUUID == ancUUID {
			// Current matches ancestor, target changed — fast-forward
			if !dryRun {
				if err := c.writeFileFromUUID(name, tgtUUID); err != nil {
					return UpdateNone, fmt.Errorf("update %s: %w", name, err)
				}
			}
			return UpdateUpdated, nil
		}

		if tgtUUID == ancUUID {
			// Target matches ancestor, current changed — keep current
			return UpdateNone, nil
		}

		// Both changed — 3-way merge needed
		return c.mergeFile(name, ancUUID, curUUID, tgtUUID, strategy, dryRun)

	default:
		// Not in current, not in target — impossible given allNames construction
		return UpdateNone, nil
	}
}

// mergeFile performs a 3-way merge of a file that was modified in both current and target.
func (c *Checkout) mergeFile(
	name, ancUUID, curUUID, tgtUUID string,
	strategy merge.Strategy,
	dryRun bool,
) (UpdateChange, error) {
	// Expand all three versions
	ancData, err := c.expandUUID(ancUUID)
	if err != nil {
		return UpdateNone, fmt.Errorf("merge %s: expand ancestor: %w", name, err)
	}
	curData, err := c.expandUUID(curUUID)
	if err != nil {
		return UpdateNone, fmt.Errorf("merge %s: expand current: %w", name, err)
	}
	tgtData, err := c.expandUUID(tgtUUID)
	if err != nil {
		return UpdateNone, fmt.Errorf("merge %s: expand target: %w", name, err)
	}

	result, err := strategy.Merge(ancData, curData, tgtData)
	if err != nil {
		return UpdateNone, fmt.Errorf("merge %s: %w", name, err)
	}

	if !dryRun {
		fullPath, err := c.safePath(name)
		if err != nil {
			return UpdateNone, fmt.Errorf("merge %s: path traversal: %w", name, err)
		}
		parentDir := filepath.Dir(fullPath)
		if err := c.env.Storage.MkdirAll(parentDir, os.FileMode(0o755)); err != nil {
			return UpdateNone, fmt.Errorf("merge %s: mkdir: %w", name, err)
		}
		if err := c.env.Storage.WriteFile(fullPath, result.Content, os.FileMode(0o644)); err != nil {
			return UpdateNone, fmt.Errorf("merge %s: write: %w", name, err)
		}
	}

	if result.Clean {
		return UpdateMerged, nil
	}
	return UpdateConflictMerged, nil
}

// writeFileFromUUID expands a blob by UUID and writes it to the checkout directory.
func (c *Checkout) writeFileFromUUID(name, uuid string) error {
	data, err := c.expandUUID(uuid)
	if err != nil {
		return fmt.Errorf("writeFileFromUUID %s: %w", name, err)
	}

	fullPath, err := c.safePath(name)
	if err != nil {
		return fmt.Errorf("writeFileFromUUID %s: path traversal: %w", name, err)
	}
	parentDir := filepath.Dir(fullPath)
	if err := c.env.Storage.MkdirAll(parentDir, os.FileMode(0o755)); err != nil {
		return fmt.Errorf("mkdir %s: %w", parentDir, err)
	}
	if err := c.env.Storage.WriteFile(fullPath, data, os.FileMode(0o644)); err != nil {
		return fmt.Errorf("write %s: %w", fullPath, err)
	}
	return nil
}

// expandUUID resolves a UUID to its expanded blob content.
func (c *Checkout) expandUUID(uuid string) ([]byte, error) {
	rid, ok := content.AvailableByUUID(c.repo.DB(), uuid)
	if !ok {
		return nil, fmt.Errorf("blob not found for uuid %s", uuid)
	}
	data, err := content.Expand(c.repo.DB(), rid)
	if err != nil {
		return nil, fmt.Errorf("expand rid %d (uuid %s): %w", rid, uuid, err)
	}
	return data, nil
}
