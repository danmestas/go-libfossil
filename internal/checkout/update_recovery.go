package checkout

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
)

// UpdateRequiredFiles resolves the update target and returns the covered target
// payloads plus any base payloads needed for a local merge. Planning scans live
// working files without storing scan results; no working bytes survive recovery.
// The caller binds target and RecoverySnapshot before an unlocked exchange.
func (c *Checkout) UpdateRequiredFiles(
	ctx context.Context, opts UpdateOpts,
) (libfossil.FslID, []manifest.FileEntry, error) {
	target, _, files, err := c.prepareUpdate(ctx, opts)
	return target, files, err
}

func (c *Checkout) prepareUpdate(
	ctx context.Context, opts UpdateOpts,
) (libfossil.FslID, []updateStep, []manifest.FileEntry, error) {
	if c == nil {
		panic("checkout.Update: nil checkout")
	}
	if ctx == nil {
		panic("checkout.Update: nil context")
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, nil, err
	}
	target, current, err := c.updateTarget(opts.TargetRID)
	if err != nil || target == 0 {
		return target, nil, nil, err
	}
	parents, err := c.mergeParents(current)
	if err != nil {
		return 0, nil, nil, err
	}
	parents = append(parents, current, target)
	if err := manifest.RequireParentContent(ctx, c.repo.DB(), parents); err != nil {
		return 0, nil, nil, err
	}
	steps, err := c.planUpdate(current, target)
	if err != nil {
		return 0, nil, nil, err
	}
	files, err := c.updateFileNeeds(ctx, target, steps)
	return target, steps, files, err
}

// updateFiles retains all target metadata, even when the worktree or store
// excludes its payload. Planning compares immutable identities, not bytes.
func (c *Checkout) updateFiles(
	target libfossil.FslID,
) ([]manifest.FileEntry, []libfossil.FslID, error) {
	files, err := manifest.ListFiles(c.repo, target)
	if err != nil {
		return nil, nil, err
	}
	rids := make([]libfossil.FslID, len(files))
	for i, file := range files {
		err := c.repo.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", file.UUID).Scan(&rids[i])
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
	}
	return files, rids, nil
}

func (c *Checkout) updateFileNeeds(
	ctx context.Context, target libfossil.FslID, steps []updateStep,
) ([]manifest.FileEntry, error) {
	files, err := manifest.ListFiles(c.repo, target)
	if err != nil {
		return nil, err
	}
	needs := make([]manifest.FileEntry, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.covers(file.Name) && !seen[file.UUID] {
			needs = append(needs, file)
			seen[file.UUID] = true
		}
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if step.action != updateMerge {
			continue
		}
		var uuid string
		err := c.repo.DB().QueryRow(
			"SELECT uuid FROM blob WHERE rid=?", step.row.RID,
		).Scan(&uuid)
		if err != nil {
			return nil, err
		}
		if !seen[uuid] {
			needs = append(needs, manifest.FileEntry{Name: step.name, UUID: uuid})
			seen[uuid] = true
		}
	}
	sort.Slice(needs, func(i, j int) bool {
		if needs[i].Name == needs[j].Name {
			return needs[i].UUID < needs[j].UUID
		}
		return needs[i].Name < needs[j].Name
	})
	return needs, nil
}

func (c *Checkout) requireUpdateFiles(ctx context.Context, files []manifest.FileEntry) error {
	var missing []string
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, available, err := content.CheckAvailableByUUID(c.repo.DB(), file.UUID)
		if err != nil {
			return err
		}
		if !available {
			missing = append(missing, file.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("checkout.Update: %w", missingContentError(missing))
	}
	return ctx.Err()
}
