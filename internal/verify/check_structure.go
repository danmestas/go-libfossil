package verify

import (
	"fmt"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// checkDeltaChains verifies delta relationships are valid.
// Detects dangling references: delta rows pointing to nonexistent blobs.
func checkDeltaChains(r *repo.Repo, report *Report) error {
	if r == nil {
		panic("checkDeltaChains: nil *repo.Repo")
	}
	if report == nil {
		panic("checkDeltaChains: nil *Report")
	}

	rows, err := r.DB().Query(`
		SELECT d.rid, d.srcid FROM delta d
		WHERE NOT EXISTS (SELECT 1 FROM blob WHERE rid = d.rid)
		   OR NOT EXISTS (SELECT 1 FROM blob WHERE rid = d.srcid)
	`)
	if err != nil {
		return fmt.Errorf("checkDeltaChains: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rid, srcid int64
		if err := rows.Scan(&rid, &srcid); err != nil {
			return fmt.Errorf("checkDeltaChains: scan: %w", err)
		}
		report.addIssue(Issue{
			Kind:    IssueDeltaDangling,
			RID:     libfossil.FslID(rid),
			UUID:    "",
			Table:   "delta",
			Message: fmt.Sprintf("delta rid=%d srcid=%d: one or both blobs missing", rid, srcid),
		})
	}
	return rows.Err()
}

// checkPhantoms verifies phantom records are properly tracked.
// Detects orphan phantoms: phantom rows with no corresponding blob.
func checkPhantoms(r *repo.Repo, report *Report) error {
	if r == nil {
		panic("checkPhantoms: nil *repo.Repo")
	}
	if report == nil {
		panic("checkPhantoms: nil *Report")
	}

	rows, err := r.DB().Query(`
		SELECT p.rid FROM phantom p
		WHERE NOT EXISTS (SELECT 1 FROM blob WHERE rid = p.rid)
	`)
	if err != nil {
		return fmt.Errorf("checkPhantoms: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return fmt.Errorf("checkPhantoms: scan: %w", err)
		}
		report.addIssue(Issue{
			Kind:    IssuePhantomOrphan,
			RID:     libfossil.FslID(rid),
			UUID:    "",
			Table:   "phantom",
			Message: fmt.Sprintf("phantom rid=%d: no corresponding blob", rid),
		})
	}
	return rows.Err()
}
