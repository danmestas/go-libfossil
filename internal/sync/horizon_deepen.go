package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// ErrNoDeepenPoint is returned by Deepen when no common ancestor of the
// store's C and the target can be established, here or by the origin: no
// ancestor of C reaches the target, so no deepen does, and C never moves to
// a check-in that is not its ancestor (draft-fossil-horizon-00 §5.8). The
// message names the remedy.
var ErrNoDeepenPoint = errors.New("horizon: no common ancestor of the horizon and the target to deepen to")

// noDeepenRemedy is the remedy §5.8 has a refused deepen name.
const noDeepenRemedy = "clone afresh with an earlier horizon to reach it"

// Deepen moves the store's horizon toward target, the full hash of a
// check-in the store needs history down to (draft-fossil-horizon-00 §5.8),
// and synchronizes at the new C' through peer. C' is an ancestor of C, so K
// only grows: target itself when it is one, else a common ancestor of C and
// target, found over the graph the store holds or asked of the origin
// (§6.2.4). The rule, the depth and the path members stay. FetchCone does
// the rest: a structural store discards the seed for C before it takes the
// one for C', and a failed transfer puts both back.
func Deepen(ctx context.Context, r *repo.Repo, peer Transport, target string, opts SyncOpts) (*SyncResult, error) {
	if ctx == nil || r == nil || peer == nil {
		panic("sync.Deepen: nil context, repo or peer")
	}
	target = strings.ToLower(target)
	if !hash.IsValidHash(target) {
		return nil, fmt.Errorf("horizon: deepen target %q is not a full artifact hash", target)
	}
	sc, err := content.StoreCone(r.DB())
	if err != nil {
		return nil, err
	}
	h, ok := horizonOf(sc)
	if !ok {
		return nil, errors.New("horizon: the store has no horizon to deepen")
	}
	point, err := deepenPoint(ctx, r, peer, h.Hash(), target, opts)
	if err != nil {
		return nil, err
	}
	nh, err := cone.NewHorizon(point, h.Rule(), h.Depth())
	if err != nil {
		return nil, err
	}
	next, err := cone.NewWithHorizon(sc.Include(), sc.Exclude(), nh)
	if err != nil {
		return nil, err
	}
	return FetchCone(ctx, r, peer, next, opts)
}

// deepenPoint is where a deepen that must reach target moves C (zit's
// deepenPoint, src/cmd/sync.zig): a common ancestor of the two, which is
// target itself when target is C's ancestor and C when target descends from
// it. The store's own graph answers when P cards it holds join the two, the
// boundary's included; otherwise the origin is asked.
func deepenPoint(ctx context.Context, r *repo.Repo, peer Transport, c, target string, opts SyncOpts) (string, error) {
	if c == target {
		return c, nil
	}
	rc, okC, err := blobRID(r, c)
	if err != nil {
		return "", err
	}
	rt, okT, err := blobRID(r, target)
	if err != nil {
		return "", err
	}
	if okC && okT {
		base, err := merge.FindCommonAncestorWithin(ctx, r, rc, rt, mergeBaseNodes)
		if errors.Is(err, merge.ErrNoCommonAncestor) {
			// The pivot search starts from check-ins only; a boundary
			// target among C's own ancestors is its own deepen point.
			base, err = boundaryAncestor(r, rc, rt)
		}
		switch {
		case err == nil:
			var uuid string
			if err := r.DB().QueryRow("SELECT uuid FROM blob WHERE rid=?", base).Scan(&uuid); err != nil {
				return "", fmt.Errorf("horizon: deepen point: %w", err)
			}
			return uuid, nil
		case !errors.Is(err, merge.ErrNoCommonAncestor) && !errors.Is(err, merge.ErrBound):
			return "", err
		}
	}
	opts.Pull, opts.Push = true, false
	opts.MergeBaseOf = MergeBaseQuery{Primary: target, Secondaries: []string{c}}
	res, err := Sync(ctx, r, peer, opts)
	if err != nil {
		return "", err
	}
	if !res.MergeBaseAnswered {
		return "", fmt.Errorf("%w: %s and %s; the origin did not answer; %s", ErrNoDeepenPoint, c, target, noDeepenRemedy)
	}
	if res.MergeBase == "" {
		return "", fmt.Errorf("%w: %s and %s; the origin names none; %s", ErrNoDeepenPoint, c, target, noDeepenRemedy)
	}
	return res.MergeBase, nil
}

// boundaryAncestor is target when the store holds no check-in for it and it
// is among c's ancestors in the plink graph the store holds, and
// merge.ErrNoCommonAncestor otherwise.
func boundaryAncestor(r *repo.Repo, c, target libfossil.FslID) (libfossil.FslID, error) {
	var found bool
	err := r.DB().QueryRow(`WITH RECURSIVE anc(rid) AS (
			SELECT ?1 UNION SELECT plink.pid FROM plink JOIN anc ON plink.cid=anc.rid LIMIT ?3)
		SELECT EXISTS(SELECT 1 FROM anc WHERE rid=?2)
			AND NOT EXISTS(SELECT 1 FROM event WHERE objid=?2 AND type='ci')`,
		c, target, mergeBaseNodes).Scan(&found)
	if err != nil {
		return 0, fmt.Errorf("horizon: deepen point: %w", err)
	}
	if !found {
		return 0, merge.ErrNoCommonAncestor
	}
	return target, nil
}

// blobRID is the rid of the artifact named uuid, held or phantom.
func blobRID(r *repo.Repo, uuid string) (libfossil.FslID, bool, error) {
	var rid libfossil.FslID
	err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("horizon: deepen point: %w", err)
	}
	return rid, true, nil
}
