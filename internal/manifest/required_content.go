package manifest

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
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
	return fmt.Sprintf("sparse required content: %d elided artifact(s); "+
		"no configured peer to resolve required artifact(s) %v", e.Elided, e.Artifacts)
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
	for i, rid := range unique {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := requiredCheckShuns(ctx, q, roots[i]); err != nil {
			return err
		}
		_, held, err := content.CheckAvailableByUUID(q, roots[i])
		if err != nil {
			return err
		}
		if !held {
			return requiredUnavailable(ctx, q, roots, roots[i], nil)
		}
		d, err := loadCheckinDeck(q, rid)
		if err != nil {
			return err
		}
		if d.B != "" {
			_, held, err := content.CheckAvailableByUUID(q, d.B)
			if err != nil {
				return err
			}
			if !held {
				return requiredUnavailable(ctx, q, roots, d.B, nil)
			}
		}
		if _, err := checkinFileCards(q, nil, d); err != nil {
			if uuid, absent := MissingSubmanifest(err); absent {
				return requiredUnavailable(ctx, q, roots, uuid, err)
			}
			return err
		}
	}
	return ctx.Err()
}

// Existing tree readers distinguish absent structure from malformed structure.
// This independent bounded walk adds the writer's visibility gate without
// changing read-only enumeration or turning elision into repository corruption.
func requiredCheckShuns(ctx context.Context, q db.Querier, root string) error {
	type entry struct {
		uuid     string
		baseline bool
	}
	stack := []entry{{uuid: root}}
	seen := make(map[entry]bool)
	for visits := 0; len(stack) > 0; visits++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if visits >= maxTreeNodes {
			return fmt.Errorf("sparse required content: structural visit limit exceeded")
		}
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[next] {
			continue
		}
		seen[next] = true
		barred, err := shun.IsShunned(q, next.uuid)
		if err != nil {
			return err
		}
		if barred {
			return fmt.Errorf("sparse required content: required artifact %s is shunned", next.uuid)
		}
		rid, held, err := content.CheckAvailableByUUID(q, next.uuid)
		if err != nil {
			return err
		}
		if !held {
			continue
		}
		data, err := expandManifestBytes(q, nil, rid)
		if err != nil {
			return err
		}
		d, err := deck.Parse(data)
		if err != nil {
			return err
		}
		if d.B != "" && !next.baseline {
			stack = append(stack, entry{uuid: d.B, baseline: true})
		}
		if len(d.S) > maxTreeNodes-len(stack) {
			return fmt.Errorf("sparse required content: structural stack limit exceeded")
		}
		for i := len(d.S) - 1; i >= 0; i-- {
			stack = append(stack, entry{uuid: d.S[i].UUID})
		}
	}
	return nil
}

func requiredUnavailable(
	ctx context.Context, q db.Querier, roots []string, uuid string, cause error,
) error {
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
	return &RequiredContentError{
		Roots: roots, Artifacts: []string{uuid}, Elided: a.Count(content.Elided), cause: cause,
	}
}
