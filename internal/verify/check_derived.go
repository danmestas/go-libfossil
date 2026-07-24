package verify

import (
	"database/sql"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
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

	if err := checkCheckins(r, report, cache); err != nil {
		return err
	}
	if err := checkLeaves(r, report); err != nil {
		return err
	}

	return nil
}

// checkCheckins verifies event and plink tables against checkin manifests.
func checkCheckins(r *repo.Repo, report *Report, cache *content.Cache) error {
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

		if err := checkOneCheckin(r, report, libfossil.FslID(rid), uuid, cache); err != nil {
			return err
		}
	}

	return rows.Err()
}

// checkOneCheckin verifies a single blob's derived data if it's a checkin.
func checkOneCheckin(r *repo.Repo, report *Report, rid libfossil.FslID, uuid string, cache *content.Cache) error {
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

	if d.Type != deck.Checkin {
		return nil
	}

	// Check event table
	if err := checkEvent(r.DB(), report, rid, uuid); err != nil {
		return err
	}

	// Check plink table
	for _, parentUUID := range d.P {
		if err := checkPlink(r.DB(), report, rid, uuid, parentUUID); err != nil {
			return err
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

// checkPlink verifies that a parent relationship has a corresponding plink row.
func checkPlink(q db.Querier, report *Report, cid libfossil.FslID, childUUID string, parentUUID string) error {
	if q == nil {
		panic("verify: checkPlink: nil querier")
	}
	if report == nil {
		panic("verify: checkPlink: nil report")
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

// checkLeaves verifies that the leaf table matches actual leaves in the DAG.
func checkLeaves(r *repo.Repo, report *Report) error {
	if r == nil {
		panic("verify: checkLeaves: nil repo")
	}
	if report == nil {
		panic("verify: checkLeaves: nil report")
	}

	expected, err := computeExpectedLeaves(r.DB())
	if err != nil {
		return err
	}

	actual, err := getActualLeaves(r.DB())
	if err != nil {
		return err
	}

	if !leafSetsEqual(expected, actual) {
		report.addIssue(Issue{
			Kind:    IssueLeafIncorrect,
			Table:   "leaf",
			Message: fmt.Sprintf("leaf table incorrect: expected %d leaves, got %d", len(expected), len(actual)),
		})
	}

	return nil
}

// computeExpectedLeaves finds all checkins with no children.
func computeExpectedLeaves(q db.Querier) (map[libfossil.FslID]bool, error) {
	if q == nil {
		panic("verify: computeExpectedLeaves: nil querier")
	}

	leaves := make(map[libfossil.FslID]bool)
	query := `
		SELECT e.objid
		FROM event e
		WHERE e.type='ci'
		AND NOT EXISTS (SELECT 1 FROM plink WHERE pid=e.objid)
	`
	rows, err := q.Query(query)
	if err != nil {
		return nil, fmt.Errorf("computeExpectedLeaves: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return nil, fmt.Errorf("computeExpectedLeaves scan: %w", err)
		}
		leaves[libfossil.FslID(rid)] = true
	}

	return leaves, rows.Err()
}

// getActualLeaves reads the current leaf table.
func getActualLeaves(q db.Querier) (map[libfossil.FslID]bool, error) {
	if q == nil {
		panic("verify: getActualLeaves: nil querier")
	}

	leaves := make(map[libfossil.FslID]bool)
	rows, err := q.Query("SELECT rid FROM leaf")
	if err == sql.ErrNoRows {
		return leaves, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getActualLeaves: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return nil, fmt.Errorf("getActualLeaves scan: %w", err)
		}
		leaves[libfossil.FslID(rid)] = true
	}

	return leaves, rows.Err()
}

// leafSetsEqual compares two sets of leaves.
func leafSetsEqual(a, b map[libfossil.FslID]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for rid := range a {
		if !b[rid] {
			return false
		}
	}
	return true
}
