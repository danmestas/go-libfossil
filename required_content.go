package libfossil

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	internalsync "github.com/danmestas/go-libfossil/internal/sync"
)

// RequiredContentOpts authorizes read-only recovery from an explicit peer.
// Neither the peer nor credentials are saved. A nil Peer keeps the operation
// local. Sync accepts authentication, project/server identity, Pull and Private;
// push, unversioned, extension-table and link-sharing options are refused.
// Existing context-free operations never discover or contact a peer.
type RequiredContentOpts struct {
	Peer Transport
	Sync SyncOpts
}

// CommitContext creates a commit, recovering required parent structure from Peer
// when necessary. Recovery runs without a write transaction and never broadens
// the store cone. The operation is retried once from the caller's original
// inputs after revalidating immutable parents and construction policy.
func (r *Repo) CommitContext(
	ctx context.Context, opts CommitOpts, required RequiredContentOpts,
) (int64, string, error) {
	if err := required.validate(ctx); err != nil {
		return 0, "", err
	}
	parents := []fsltype.FslID{fsltype.FslID(opts.ParentID)}
	for _, parent := range opts.MergeParents {
		parents = append(parents, fsltype.FslID(parent))
	}
	state, err := requiredState(r.inner, parents)
	if err != nil {
		return 0, "", err
	}
	err = manifest.RequireParentContent(ctx, r.inner.DB(), parents)
	if err != nil {
		if err := required.recover(ctx, r.inner, err); err != nil {
			return 0, "", err
		}
		if err := checkRequiredState(r.inner, parents, state); err != nil {
			return 0, "", err
		}
	}
	if err := required.commitFiles(ctx, r.inner, opts); err != nil {
		return 0, "", err
	}
	if err := checkRequiredState(r.inner, parents, state); err != nil {
		return 0, "", err
	}
	return r.commit(ctx, opts)
}

func (o RequiredContentOpts) validate(ctx context.Context) error {
	if ctx == nil {
		panic("libfossil: required content: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s := o.Sync
	if s.Push || s.UV || s.XTableSync || s.ShareLinks || s.MaxSend != 0 || s.PeerID != "" {
		return fmt.Errorf("libfossil: required-content recovery is read-only; incompatible sync options")
	}
	if s.Observer != nil || s.Buggify != nil {
		return fmt.Errorf("libfossil: required-content recovery does not support sync instrumentation")
	}
	return nil
}

func (o RequiredContentOpts) internalSync() internalsync.SyncOpts {
	return internalsync.SyncOpts{
		Pull: true, ProjectCode: o.Sync.ProjectCode, ServerCode: o.Sync.ServerCode,
		User: o.Sync.User, Password: o.Sync.Password, Private: o.Sync.Private,
	}
}

func (o RequiredContentOpts) recover(ctx context.Context, r *repo.Repo, refusal error) error {
	var required *manifest.RequiredContentError
	if !errors.As(refusal, &required) {
		return refusal
	}
	if o.Peer == nil {
		return refusal
	}
	_, err := internalsync.FetchRequiredArtifacts(ctx, r, &transportAdapter{pub: o.Peer},
		required.Roots, o.internalSync())
	return err
}

// Only recovery-relevant construction settings are captured: receiving SHA3
// content may legitimately promote auto hash policy through ordinary ingestion.
func requiredState(r *repo.Repo, parents []fsltype.FslID) (string, error) {
	c, err := content.StoreCone(r.DB())
	if err != nil {
		return "", err
	}
	var state strings.Builder
	if c != nil {
		state.WriteString(c.Digest())
	}
	for _, name := range []string{manifest.SettingEnableSubmanifests, manifest.SettingSubmanifestK} {
		var value string
		if err := r.DB().QueryRow(
			"SELECT COALESCE((SELECT value FROM config WHERE name=?),'')", name,
		).Scan(&value); err != nil {
			return "", err
		}
		fmt.Fprintf(&state, "\n%s=%s", name, value)
	}
	for _, rid := range parents {
		if rid <= 0 {
			continue
		}
		var uuid string
		if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			return "", err
		}
		fmt.Fprintf(&state, "\n%d=%s", rid, uuid)
	}
	return state.String(), nil
}

func checkRequiredState(r *repo.Repo, parents []fsltype.FslID, before string) error {
	after, err := requiredState(r, parents)
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("libfossil: snapshot or policy changed during required-content recovery")
	}
	return nil
}
