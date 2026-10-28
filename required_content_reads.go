package libfossil

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/content"
	internalsync "github.com/danmestas/go-libfossil/internal/sync"
)

// ReadFileContext is ReadFile that, when the check-in's file lies below the
// store cone's horizon and the store does not hold it, fetches exactly the
// artifacts the read needs from Peer and reads again: the payload, and for a
// structural store first the check-in's manifest, baseline and nodes. This is
// the targeted demand fetch of draft-fossil-horizon-00 §4.4: the store cone
// and its horizon do not move, and what is fetched is held content a later
// CompactStore may drop. Without a Peer the read fails as ReadFile does,
// matching ErrBelowHorizon and naming the horizon. A read needing more than
// one demand fetch carries is refused; DeepenHorizon is its remedy.
func (r *Repo) ReadFileContext(
	ctx context.Context, rid int64, filePath string, required RequiredContentOpts,
) ([]byte, error) {
	if err := required.validate(ctx); err != nil {
		return nil, err
	}
	var data []byte
	err := r.demandFetch(ctx, required, func() error {
		var err error
		data, err = r.readFile(ctx, rid, filePath)
		return err
	})
	return data, err
}

// ReadFileAtContext is ReadFileAt with ReadFileContext's demand fetch. The
// version is resolved locally; a structural store resolves a check-in below
// its horizon by the full hash its phantom carries.
func (r *Repo) ReadFileAtContext(
	ctx context.Context, version string, filePath string, required RequiredContentOpts,
) ([]byte, error) {
	if err := required.validate(ctx); err != nil {
		return nil, err
	}
	rid, err := r.ResolveVersion(version)
	if err != nil {
		return nil, fmt.Errorf("libfossil: read file at %q: %w", version, err)
	}
	return r.ReadFileContext(ctx, rid, filePath, required)
}

// AnnotateContext is Annotate with ReadFileContext's demand fetch: every
// version of the file the walk needs below the horizon, along the primary
// chain, is fetched in one round before the walk runs again, so lines are
// attributed to the check-ins below the horizon that added them. A walk
// that crosses a structural horizon still reports the lines it cannot
// attribute as BelowHorizon, since naming those check-ins is a deepen.
func (r *Repo) AnnotateContext(
	ctx context.Context, opts AnnotateOpts, required RequiredContentOpts,
) ([]AnnotatedLine, error) {
	if err := required.validate(ctx); err != nil {
		return nil, err
	}
	var lines []AnnotatedLine
	err := r.demandFetch(ctx, required, func() error {
		var err error
		lines, err = r.Annotate(opts)
		return err
	})
	return lines, err
}

// DiffContext is Diff with ReadFileContext's demand fetch: what both sides
// lack below the horizon is fetched in one round.
func (r *Repo) DiffContext(
	ctx context.Context, ridA, ridB int64, filePath string, required RequiredContentOpts,
) ([]DiffEntry, error) {
	if err := required.validate(ctx); err != nil {
		return nil, err
	}
	var entries []DiffEntry
	err := r.demandFetch(ctx, required, func() error {
		var err error
		entries, err = r.diff(ctx, ridA, ridB, filePath)
		return err
	})
	return entries, err
}

// demandFetch runs read and, when it fails for content below the horizon
// that required.Peer may supply, fetches exactly what the refusal names and
// runs read again. A structural store's refusal can name only the check-ins
// whose structure it lacks, since their manifests name the payloads, so a
// read that fetched structure may fetch once more for the payloads: at most
// two fetches and three runs. Neither fetch changes the store cone.
func (r *Repo) demandFetch(ctx context.Context, required RequiredContentOpts, read func() error) error {
	if ctx == nil || read == nil {
		panic("libfossil.demandFetch: nil context or read")
	}
	err := read()
	for fetches := 0; fetches < 2; fetches++ {
		var missing *content.MissingBelowHorizon
		if err == nil || required.Peer == nil || !errors.As(err, &missing) {
			return err
		}
		if fetches > 0 && len(missing.Checkins) > 0 {
			return err
		}
		if n := len(missing.Checkins) + len(missing.Names); n > content.DemandFetchLimit {
			return fmt.Errorf("libfossil: %d artifacts exceed one demand fetch (%d); "+
				"deepen the horizon to the check-in instead (DeepenHorizon): %w",
				n, content.DemandFetchLimit, err)
		}
		if len(missing.Checkins) > 0 {
			_, ferr := internalsync.FetchRequiredArtifacts(ctx, r.inner, &transportAdapter{pub: required.Peer},
				missing.Checkins, required.internalSync())
			if ferr != nil {
				return fmt.Errorf("libfossil: demand fetch: %w", ferr)
			}
		}
		if err := required.recoverFiles(ctx, r.inner, missing.Names); err != nil {
			return fmt.Errorf("libfossil: demand fetch: %w", err)
		}
		structural := len(missing.Checkins) > 0
		err = read()
		if !structural {
			return err
		}
	}
	return err
}

// joinMissing folds err into acc when it is content missing below the
// horizon, and returns any other error unchanged.
func joinMissing(acc *content.MissingBelowHorizon, err error) (*content.MissingBelowHorizon, error) {
	var missing *content.MissingBelowHorizon
	if err == nil || !errors.As(err, &missing) {
		return acc, err
	}
	return acc.Join(missing), nil
}
