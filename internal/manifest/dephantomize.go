package manifest

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// AfterDephantomize crosslinks a formerly-phantom blob and any dependents.
// Matches Fossil's after_dephantomize (content.c:389-456).
//
// ctx bounds the graph walk: this cascade runs synchronously mid-round as a
// clone fills phantoms (see sync.storeReceivedFile), and on a large delta/orphan
// graph can run for many multiples of a clone's deadline. A cancelled ctx aborts
// the walk within crosslinkCancelCheckStride steps (see afterDephantomize),
// mirroring the whole-repository sweep's CrosslinkContext contract. Callers with
// no deadline pass context.Background().
func AfterDephantomize(ctx context.Context, r *repo.Repo, rid libfossil.FslID) {
	if r == nil {
		panic("manifest.AfterDephantomize: r must not be nil")
	}
	if rid <= 0 {
		return
	}
	afterDephantomize(ctx, r, rid, true)
}

func afterDephantomize(ctx context.Context, r *repo.Repo, rid libfossil.FslID, linkFlag bool) {
	// Work stack replaces recursion. Bounded by total blob count in repo.
	type workItem struct {
		rid      libfossil.FslID
		linkFlag bool
	}
	stack := []workItem{{rid: rid, linkFlag: linkFlag}}

	const maxIterations = 1_000_000 // Guard against pathological delta chains.
	iterations := 0

	for len(stack) > 0 {
		// Observe the deadline every crosslinkCancelCheckStride pops, mirroring
		// linkBatch's stride-batched poll in the whole-repository sweep. This
		// cascade runs synchronously mid-round as a clone fills phantoms (see
		// sync.storeReceivedFile), so without this a cancelled clone context
		// could not interrupt it until the walk finished or hit maxIterations --
		// the deadline-blind overshoot of issue #166. On cancellation we return
		// what has been crosslinked so far, exactly as the batch sweep abandons
		// its remaining candidates; nothing pretends the whole cascade completed.
		if iterations%crosslinkCancelCheckStride == 0 {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		iterations++
		if iterations > maxIterations {
			return // Safety bound exceeded.
		}

		// Pop from stack.
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		current := item.rid

		if current <= 0 {
			continue
		}

		if item.linkFlag {
			if err := crosslinkSingle(ctx, r, current); err != nil {
				slog.Warn("manifest.AfterDephantomize: crosslink failed, blob left un-crosslinked",
					"rid", current, "error", err)
			}
		}

		// Process orphaned delta manifests whose baseline is this rid.
		orphanRows, err := r.DB().Query("SELECT rid FROM orphan WHERE baseline=?", current)
		if err == nil {
			var orphans []libfossil.FslID
			for orphanRows.Next() {
				var orid int64
				if orphanRows.Scan(&orid) == nil {
					orphans = append(orphans, libfossil.FslID(orid))
				}
			}
			orphanRows.Close()
			for _, orid := range orphans {
				if err := crosslinkSingle(ctx, r, orid); err != nil {
					slog.Warn("manifest.AfterDephantomize: crosslink of orphan failed, blob left un-crosslinked",
						"rid", orid, "baseline", current, "error", err)
				}
			}
			if len(orphans) > 0 {
				if _, err := r.DB().Exec("DELETE FROM orphan WHERE baseline=?", current); err != nil {
					continue
				}
			}
		}

		// Find delta children not yet crosslinked.
		childRows, err := r.DB().Query(
			`SELECT rid FROM delta WHERE srcid=? AND NOT EXISTS (SELECT 1 FROM mlink WHERE mid=delta.rid)`, current)
		if err != nil {
			continue
		}
		var children []libfossil.FslID
		for childRows.Next() {
			var crid int64
			if childRows.Scan(&crid) == nil {
				children = append(children, libfossil.FslID(crid))
			}
		}
		childRows.Close()

		// Push all children onto work stack (reverse order for LIFO processing).
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, workItem{rid: children[i], linkFlag: true})
		}
	}
}

// crosslinkSingle crosslinks a single blob by rid.
//
// ctx is threaded for signature consistency with the AfterDephantomize call
// chain; the cancellation poll lives in afterDephantomize's work-stack loop
// (stride-batched, like linkBatch) rather than here, so a filled phantom is not
// charged a channel read per artifact.
func crosslinkSingle(ctx context.Context, r *repo.Repo, rid libfossil.FslID) error {
	if r == nil {
		panic("crosslinkSingle: r must not be nil")
	}
	if rid <= 0 {
		panic("crosslinkSingle: rid must be positive")
	}

	// Bare Expand: crosslinks one just-dephantomized artifact, expanded once.
	// The whole-repository crosslink sweep that has genuine chain overlap is
	// Crosslink/CrosslinkContext, which already runs its own content.Cache.
	data, err := content.Expand(r.DB(), rid)
	if err != nil {
		return fmt.Errorf("crosslinkSingle expand rid=%d: %w", rid, err)
	}
	d, err := deck.Parse(data)
	if err != nil {
		return fmt.Errorf("crosslinkSingle parse rid=%d: %w", rid, err)
	}
	// The crosslink helpers write on a *db.Tx so the whole-repository sweep can
	// run in one transaction (see CrosslinkContext). This single-blob path is
	// not that sweep, so it wraps its one artifact's writes in their own
	// transaction, keeping event/plink/mlink/tagxref for the blob atomic.
	return r.WithTx(func(tx *db.Tx) error {
		switch d.Type {
		case deck.Checkin:
			// nil cache: this is the single-artifact path, not the sweep, so the
			// parent manifest is expanded directly rather than through the sweep's
			// shared content cache.
			return crosslinkCheckin(tx, rid, d, nil)
		case deck.Wiki:
			_, err := crosslinkWiki(tx, rid, d)
			return err
		case deck.Ticket:
			_, err := crosslinkTicket(tx, rid, d)
			return err
		case deck.Event:
			_, err := crosslinkEvent(tx, rid, d)
			return err
		case deck.Attachment:
			return crosslinkAttachment(tx, rid, d)
		case deck.Cluster:
			return CrosslinkCluster(tx, rid, d)
		case deck.ForumPost:
			return crosslinkForum(tx, rid, d)
		case deck.Control:
			return crosslinkControl(tx, rid, d)
		}
		return nil
	})
}
