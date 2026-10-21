package libfossil

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/shun"
	internalsync "github.com/danmestas/go-libfossil/internal/sync"
)

// requiredFileNames checks only operation-selected payloads, never every F card
// reachable from a recovered parent. Held shunned content is still forbidden.
func requiredFileNames(
	ctx context.Context, r *repo.Repo, files []manifest.FileEntry, need func(string) bool,
) ([]string, error) {
	var names []string
	seen := make(map[string]bool)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !need(file.Name) || seen[file.UUID] {
			continue
		}
		seen[file.UUID] = true
		barred, err := shun.IsShunned(r.DB(), file.UUID)
		if err != nil {
			return nil, err
		}
		if barred {
			return nil, fmt.Errorf("libfossil: required file payload %s is shunned", file.UUID)
		}
		_, held, err := content.CheckAvailableByUUID(r.DB(), file.UUID)
		if err != nil {
			return nil, err
		}
		if !held {
			if len(names) >= 1<<14 {
				return nil, fmt.Errorf("libfossil: required file payload limit exceeded")
			}
			names = append(names, file.UUID)
		}
	}
	return names, nil
}

func (o RequiredContentOpts) commitFiles(ctx context.Context, r *repo.Repo, opts CommitOpts) error {
	if opts.ParentID <= 0 || opts.PartialManifest {
		return ctx.Err()
	}
	files, err := manifest.ListFiles(r, fsltype.FslID(opts.ParentID))
	if err != nil {
		return err
	}
	coverage, err := content.StoreCone(r.DB())
	if err != nil {
		return err
	}
	supplied := make(map[string]bool, len(opts.Files))
	for _, file := range opts.Files {
		supplied[file.Name] = true
	}
	names, err := requiredFileNames(ctx, r, files, func(name string) bool {
		return !supplied[name] && (coverage == nil || coverage.Covers(name))
	})
	if err != nil {
		return err
	}
	return o.recoverFiles(ctx, r, names)
}

func (o RequiredContentOpts) checkinFiles(
	ctx context.Context, c *Checkout, opts CheckoutCommitOpts,
) error {
	parent, _, err := c.inner.Version()
	if err != nil {
		return err
	}
	if parent == 0 {
		return ctx.Err()
	}
	files, err := c.inner.CommitCarriedFiles(ctx, opts.Files)
	if err != nil {
		return err
	}
	// The selector names exactly what the checkout writer consumes, including
	// when its materialized coverage exceeds the repository's retention cone.
	names, err := requiredFileNames(ctx, c.inner.Repo(), files, func(string) bool { return true })
	if err != nil {
		return err
	}
	return o.recoverFiles(ctx, c.inner.Repo(), names)
}

func (o RequiredContentOpts) recoverFiles(
	ctx context.Context, r *repo.Repo, names []string,
) error {
	if len(names) == 0 {
		return ctx.Err()
	}
	if o.Peer == nil {
		absences, err := content.LoadAbsencesContext(ctx, r.DB())
		if err != nil {
			return err
		}
		return fmt.Errorf("libfossil: required in-cone file payloads: %d artifact(s), "+
			"%d elided artifact(s); no configured peer", len(names), absences.Count(content.Elided))
	}
	_, err := internalsync.FetchRequiredFiles(ctx, r, &transportAdapter{pub: o.Peer},
		names, o.internalSync())
	return err
}
