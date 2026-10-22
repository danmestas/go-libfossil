package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/horizon"
)

// horizonAbove returns the blobs above the content horizon h
// (draft-fossil-horizon-00 §4.2): every F-card target of a check-in in the
// held set K, read from the store's own manifests. Nothing is stored: K and
// this set are recomputed on every classification (§3.1, §3.2).
//
// placed is false when C is not a check-in the store holds. Such a horizon
// elides nothing, as zit's classification does (src/sparse.zig,
// ClassifyState.extend, at horizon-rfc e6ee276c89): reading K as empty
// would elide every blob, and after a deepen to a C' not yet pulled the
// pull that brings C' would then ask for none of the blobs it must keep.
// No blob can be proven below a horizon the store cannot place; the cost
// of keeping them wanted is bandwidth, never data.
func horizonAbove(
	ctx context.Context, q db.Querier, facts *ConeReferenceFacts, h cone.Horizon,
) (above map[string]bool, placed bool, err error) {
	if ctx == nil || q == nil || facts == nil {
		panic("content.horizonAbove: nil context, querier or facts")
	}
	var c libfossil.FslID
	err = q.QueryRow("SELECT rid FROM blob WHERE uuid=?", h.Hash()).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("content.horizonAbove: C: %w", err)
	}
	k, err := horizon.HeldSet(ctx, q, c, h.Rule())
	if errors.Is(err, horizon.ErrNotHeld) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	above, err = facts.treeFiles(ctx, k)
	return above, err == nil, err
}

// treeFiles returns every F-card target that the check-ins roots name,
// directly, through their B baselines, or through S sub-manifest nodes.
// mlink cannot answer this: it records a file version only at the check-in
// that changed it, while a check-in's tree names every file it carries.
//
// One visited set over structural names reads a baseline or node shared by
// many check-ins once, so the walk is linear in K's distinct structure, and
// bounded like the repository's other tree walks. A delta manifest's
// deletions and overrides are not applied: a blob its baseline names counts
// as named, as in zit. Keeping more than the exact tree is sound, and it
// keeps this a plain reachability walk. A tree piece the store does not
// hold is skipped; it is itself a transit phantom, and the classification
// after it arrives counts the blobs it names.
func (facts *ConeReferenceFacts) treeFiles(
	ctx context.Context, roots []libfossil.FslID,
) (map[string]bool, error) {
	files := map[string]bool{}
	visited := map[string]bool{}
	var work []string
	push := func(uuid string) {
		if uuid != "" && !visited[uuid] {
			visited[uuid] = true
			work = append(work, uuid)
		}
	}
	for _, rid := range roots {
		push(facts.names[rid])
	}
	for len(work) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(visited) > coneReferenceNodes {
			return nil, fmt.Errorf("horizon: tree walk bound exceeded")
		}
		uuid := work[len(work)-1]
		work = work[:len(work)-1]
		d := facts.manifests[uuid]
		if d == nil {
			d = facts.byUUID[uuid]
		}
		if d == nil {
			continue
		}
		for _, f := range d.F {
			if f.UUID != "" {
				files[f.UUID] = true
			}
		}
		push(d.B)
		for _, s := range d.S {
			push(s.UUID)
		}
	}
	return files, nil
}
