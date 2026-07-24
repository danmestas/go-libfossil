package manifest

import (
	"fmt"
	"log/slog"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// AfterDephantomize crosslinks a formerly-phantom blob and any dependents.
// Matches Fossil's after_dephantomize (content.c:389-456).
func AfterDephantomize(r *repo.Repo, rid libfossil.FslID) {
	if r == nil {
		panic("manifest.AfterDephantomize: r must not be nil")
	}
	if rid <= 0 {
		return
	}
	afterDephantomize(r, rid, true)
}

func afterDephantomize(r *repo.Repo, rid libfossil.FslID, linkFlag bool) {
	// Work stack replaces recursion. Bounded by total blob count in repo.
	type workItem struct {
		rid      libfossil.FslID
		linkFlag bool
	}
	stack := []workItem{{rid: rid, linkFlag: linkFlag}}

	const maxIterations = 1_000_000 // Guard against pathological delta chains.
	iterations := 0

	for len(stack) > 0 {
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
			if err := crosslinkSingle(r, current); err != nil {
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
				if err := crosslinkSingle(r, orid); err != nil {
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
func crosslinkSingle(r *repo.Repo, rid libfossil.FslID) error {
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
	switch d.Type {
	case deck.Checkin:
		return crosslinkCheckin(r, rid, d)
	case deck.Wiki:
		_, err = crosslinkWiki(r, rid, d)
		return err
	case deck.Ticket:
		_, err = crosslinkTicket(r, rid, d)
		return err
	case deck.Event:
		_, err = crosslinkEvent(r, rid, d)
		return err
	case deck.Attachment:
		return crosslinkAttachment(r, rid, d)
	case deck.Cluster:
		return CrosslinkCluster(r.DB(), rid, d)
	case deck.ForumPost:
		return crosslinkForum(r, rid, d)
	case deck.Control:
		return crosslinkControl(r, rid, d)
	}
	return nil
}
