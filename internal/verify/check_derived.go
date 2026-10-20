package verify

import (
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// checkDerived verifies consistency of derived tables: event, mlink, plink, tagxref, filename, leaf.
func checkDerived(r *repo.Repo, report *Report, cache *content.Cache) error {
	if r == nil {
		panic("verify: checkDerived: nil repo")
	}
	if report == nil {
		panic("verify: checkDerived: nil report")
	}

	// detached collects the parents a P-card or a parent tag names that the
	// check-in's plink no longer holds, which checkLeaves must not hold to a
	// leaf verdict.
	detached := make(map[libfossil.FslID]struct{})
	if err := checkCheckins(r, report, cache, detached); err != nil {
		return err
	}
	if err := checkLeaves(r, report, detached); err != nil {
		return err
	}

	return nil
}

// checkCheckins verifies event and plink tables against checkin manifests.
func checkCheckins(r *repo.Repo, report *Report, cache *content.Cache, detached map[libfossil.FslID]struct{}) error {
	if r == nil {
		panic("verify: checkCheckins: nil repo")
	}
	if report == nil {
		panic("verify: checkCheckins: nil report")
	}

	rows, err := r.DB().Query("SELECT rid, uuid FROM blob WHERE size >= 0")
	if err != nil {
		return fmt.Errorf("checkCheckins query blobs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rid int64
		var uuid string
		if err := rows.Scan(&rid, &uuid); err != nil {
			return fmt.Errorf("checkCheckins scan: %w", err)
		}

		if err := checkOneCheckin(r, report, libfossil.FslID(rid), uuid, cache, detached); err != nil {
			return err
		}
	}

	return rows.Err()
}

// checkOneCheckin verifies a single blob's derived data if it's a checkin.
func checkOneCheckin(
	r *repo.Repo, report *Report, rid libfossil.FslID, uuid string, cache *content.Cache,
	detached map[libfossil.FslID]struct{},
) error {
	if r == nil {
		panic("verify: checkOneCheckin: nil repo")
	}
	if report == nil {
		panic("verify: checkOneCheckin: nil report")
	}

	data, err := cache.Expand(r.DB(), rid)
	if err != nil {
		// Not expandable - skip silently (might be corrupt, already reported)
		return nil
	}

	d, err := deck.Parse(data)
	if err != nil {
		// Not a valid manifest - skip silently
		return nil
	}

	if err := collectReparents(r.DB(), d, uuid, detached); err != nil {
		return err
	}
	if d.Type != deck.Checkin {
		return nil
	}

	// Check event table
	if err := checkEvent(r.DB(), report, rid, uuid); err != nil {
		return err
	}

	// Check plink table. A parent tag replaces the rows the P-card gave
	// (fossil's manifest_reparent_checkin), and cancelling it re-derives
	// nothing, so a check-in carrying one may lack any of them.
	var reparented int
	err = r.DB().QueryRow(`SELECT count(*) FROM tagxref JOIN tag USING(tagid)
	                        WHERE tag.tagname='parent' AND tagxref.rid=?`, rid).Scan(&reparented)
	if err != nil {
		return fmt.Errorf("checkOneCheckin parent tag: %w", err)
	}
	for _, parentUUID := range d.P {
		if err := checkPlink(r.DB(), report, rid, uuid, parentUUID, reparented > 0, detached); err != nil {
			return err
		}
	}

	return nil
}

// collectReparents adds to detached each parent that a parent tag in d names
// and its target's plink does not hold. A later parent tag replaces an
// earlier one's tagxref row, so only the artifacts still record a parent an
// earlier reparent linked and a later one dropped.
func collectReparents(q db.Querier, d *deck.Deck, uuid string, detached map[libfossil.FslID]struct{}) error {
	if q == nil {
		panic("verify: collectReparents: nil querier")
	}
	if d == nil {
		panic("verify: collectReparents: nil deck")
	}
	if detached == nil {
		panic("verify: collectReparents: nil detached")
	}
	for _, t := range d.T {
		if t.Name != "parent" || t.Type != deck.TagSingleton {
			continue
		}
		target := t.UUID
		if target == "*" {
			target = uuid
		}
		cid, ok := blob.Exists(q, target)
		if !ok {
			continue
		}
		for _, parent := range strings.Fields(t.Value) {
			pid, ok := blob.Exists(q, parent)
			if !ok {
				continue
			}
			var count int
			err := q.QueryRow("SELECT COUNT(*) FROM plink WHERE pid = ? AND cid = ?", pid, cid).Scan(&count)
			if err != nil {
				return fmt.Errorf("collectReparents query: %w", err)
			}
			if count == 0 {
				detached[pid] = struct{}{}
			}
		}
	}
	return nil
}

// checkEvent verifies that a checkin has a corresponding event row.
func checkEvent(q db.Querier, report *Report, rid libfossil.FslID, uuid string) error {
	if q == nil {
		panic("verify: checkEvent: nil querier")
	}
	if report == nil {
		panic("verify: checkEvent: nil report")
	}

	var count int
	err := q.QueryRow("SELECT COUNT(*) FROM event WHERE objid = ?", rid).Scan(&count)
	if err != nil {
		return fmt.Errorf("checkEvent query: %w", err)
	}

	if count == 0 {
		report.addIssue(Issue{
			Kind:    IssueEventMissing,
			RID:     rid,
			UUID:    uuid,
			Table:   "event",
			Message: fmt.Sprintf("checkin rid=%d uuid=%s missing event row", rid, uuid),
		})
	}

	return nil
}

// checkPlink verifies that a parent relationship has a corresponding plink row,
// adding a parent that lacks one to detached. A missing row is an issue only
// when the child is not reparented.
func checkPlink(
	q db.Querier, report *Report, cid libfossil.FslID, childUUID string, parentUUID string,
	reparented bool, detached map[libfossil.FslID]struct{},
) error {
	if q == nil {
		panic("verify: checkPlink: nil querier")
	}
	if report == nil {
		panic("verify: checkPlink: nil report")
	}
	if detached == nil {
		panic("verify: checkPlink: nil detached")
	}

	pid, ok := blob.Exists(q, parentUUID)
	if !ok {
		// Parent doesn't exist in blob table - might be phantom
		return nil
	}

	var count int
	err := q.QueryRow("SELECT COUNT(*) FROM plink WHERE pid = ? AND cid = ?", pid, cid).Scan(&count)
	if err != nil {
		return fmt.Errorf("checkPlink query: %w", err)
	}

	if count == 0 {
		detached[pid] = struct{}{}
		if reparented {
			return nil
		}
		report.addIssue(Issue{
			Kind:    IssuePlinkMissing,
			RID:     cid,
			UUID:    childUUID,
			Table:   "plink",
			Message: fmt.Sprintf("checkin rid=%d uuid=%s missing plink to parent %s", cid, childUUID, parentUUID),
		})
	}

	return nil
}

// checkLeaves reports the leaf-table errors leaf.Audit can decide. Section 7.3
// lets the table depend on the order artifacts arrived in, so Verify does not
// recompute one expected table: it reports a row that a recheck has removed
// and a tagged check-in that a recheck has inserted, and a row no recheck
// writes. detached are the parents a reparent may have taken a child from, as
// checkPlink and collectReparents found them.
func checkLeaves(r *repo.Repo, report *Report, detached map[libfossil.FslID]struct{}) error {
	if r == nil {
		panic("verify: checkLeaves: nil repo")
	}
	if report == nil {
		panic("verify: checkLeaves: nil report")
	}

	f, err := leaf.Audit(r.DB(), detached)
	if err != nil {
		return fmt.Errorf("checkLeaves: %w", err)
	}
	for _, rid := range f.Stale {
		report.addIssue(Issue{
			Kind:    IssueLeafIncorrect,
			RID:     rid,
			Table:   "leaf",
			Message: fmt.Sprintf("leaf rid=%d has a same-branch child that carries a branch tag", rid),
		})
	}
	for _, rid := range f.Stray {
		report.addIssue(Issue{
			Kind:    IssueLeafIncorrect,
			RID:     rid,
			Table:   "leaf",
			Message: fmt.Sprintf("leaf rid=%d is no check-in a leaf recheck reaches", rid),
		})
	}
	for _, rid := range f.Missing {
		report.addIssue(Issue{
			Kind:    IssueLeafIncorrect,
			RID:     rid,
			Table:   "leaf",
			Message: fmt.Sprintf("leaf missing rid=%d, which carries a branch tag and has no same-branch child", rid),
		})
	}
	return nil
}
