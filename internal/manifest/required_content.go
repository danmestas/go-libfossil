package manifest

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/shun"
)

// RequiredContentError is a local operation's refusal, not repository damage.
// Roots identifies immutable parent manifests whose S/B closure an explicitly
// configured peer must supply. Artifacts identifies the first unavailable node;
// it is not a complete closure and must not be used as a fixed fetch radius.
// Recovery runs outside the operation's transaction and worktree locks, then
// retries the operation once with fresh readiness checks.
type RequiredContentError struct {
	Roots     []string
	Artifacts []string
	Elided    int
	cause     error
}

func (e *RequiredContentError) Error() string {
	return fmt.Sprintf("sparse required content: %d elided artifact(s); no configured peer to resolve required artifact(s) %v", e.Elided, e.Artifacts)
}

func (e *RequiredContentError) Unwrap() error { return e.cause }

// RequireParentContent validates the full structural file sets needed for
// canonical reconstruction and parent comparisons. It reads no F payloads and
// trusts no inferred parent k: full reconstruction is the conservative proof
// when no derivation evidence bound to that parent and requested k exists.
func RequireParentContent(ctx context.Context, q db.Querier, parents []libfossil.FslID) error {
	if ctx == nil || q == nil {
		panic("manifest.RequireParentContent: nil context or querier")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var roots []string
	seen := make(map[libfossil.FslID]bool)
	var unique []libfossil.FslID
	for _, rid := range parents {
		if rid <= 0 || seen[rid] {
			continue
		}
		seen[rid] = true
		var uuid string
		if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			return fmt.Errorf("parent identity %d: %w", rid, err)
		}
		roots = append(roots, uuid)
		unique = append(unique, rid)
	}
	unavailable := func(uuid string, cause error) error {
		barred, err := shun.IsShunned(q, uuid)
		if err != nil {
			return err
		}
		if barred {
			return fmt.Errorf("sparse required content: required artifact %s is shunned", uuid)
		}
		a, err := content.LoadAbsencesContext(ctx, q)
		if err != nil {
			return err
		}
		return &RequiredContentError{Roots: roots, Artifacts: []string{uuid}, Elided: a.Count(content.Elided), cause: cause}
	}
	for i, rid := range unique {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, held := content.AvailableByUUID(q, roots[i]); !held {
			return unavailable(roots[i], nil)
		}
		d, err := loadCheckinDeck(q, rid)
		if err != nil {
			return err
		}
		if d.B != "" {
			if _, held := content.AvailableByUUID(q, d.B); !held {
				return unavailable(d.B, nil)
			}
		}
		if _, err := checkinFileCards(q, nil, d); err != nil {
			if uuid, absent := MissingSubmanifest(err); absent {
				return unavailable(uuid, err)
			}
			return err
		}
	}
	return ctx.Err()
}
