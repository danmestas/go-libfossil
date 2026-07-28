package manifest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	_ "github.com/danmestas/go-libfossil/internal/testdriver"
)

// TestCrosslinkSweepBoundedByOutstandingWork pins the property issue #202 was
// the absence of: a sweep costs what is left to link, not what the repository
// holds. Ordinary file content carries none of the derived rows a crosslink
// writes, so before the fix every file blob was re-expanded and re-parsed on
// every sweep -- 39,727 of the Fossil SCM repository's 67,615 blobs, linking
// zero of them, on every sync.
//
// The assertion is on the candidate set rather than on wall clock: a timing
// bound would be flaky on shared CI, while "a converged repository offers the
// sweep nothing to do" is the invariant that actually broke.
func TestCrosslinkSweepBoundedByOutstandingWork(t *testing.T) {
	r := setupTestRepo(t)

	// The body of any repository: file content, none of it an artifact.
	const fileBlobs = 200
	for i := 0; i < fileBlobs; i++ {
		if _, _, err := blob.Store(r.DB(), []byte(fmt.Sprintf("file content %d\n", i))); err != nil {
			t.Fatalf("blob.Store: %v", err)
		}
	}
	if err := ensureNonArtifactTable(r.DB()); err != nil {
		t.Fatalf("ensureNonArtifactTable: %v", err)
	}

	before, err := collectCrosslinkCandidates(r.DB())
	if err != nil {
		t.Fatalf("collectCrosslinkCandidates: %v", err)
	}
	if len(before) < fileBlobs {
		t.Fatalf("candidates before first sweep = %d, want >= %d", len(before), fileBlobs)
	}

	if _, err := Crosslink(r); err != nil {
		t.Fatalf("Crosslink: %v", err)
	}

	after, err := collectCrosslinkCandidates(r.DB())
	if err != nil {
		t.Fatalf("collectCrosslinkCandidates: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("candidates after sweep = %d, want 0: the sweep re-examines blobs it has already ruled out", len(after))
	}

	// One new blob costs one candidate -- not one per blob already stored.
	if _, _, err := blob.Store(r.DB(), []byte("newly arrived content\n")); err != nil {
		t.Fatalf("blob.Store: %v", err)
	}
	next, err := collectCrosslinkCandidates(r.DB())
	if err != nil {
		t.Fatalf("collectCrosslinkCandidates: %v", err)
	}
	if len(next) != 1 {
		t.Fatalf("candidates after one new blob = %d, want 1", len(next))
	}
}

// TestCrosslinkCandidateQueryHasNoCorrelatedSubquery pins the other half of
// #202: the candidate query's exclusions must be answerable from an index or
// from a set materialized once, never by rescanning a derived table per blob.
// As correlated NOT EXISTS subqueries, tagxref -- whose indexes are on
// (rid, tagid) and (tagid, mtime), neither covering srcid -- was scanned in
// full for every blob, which was 99 s of the 164 s a no-op sync took.
func TestCrosslinkCandidateQueryHasNoCorrelatedSubquery(t *testing.T) {
	r := setupTestRepo(t)
	if err := ensureForumPostTable(r.DB()); err != nil {
		t.Fatalf("ensureForumPostTable: %v", err)
	}
	if err := ensureNonArtifactTable(r.DB()); err != nil {
		t.Fatalf("ensureNonArtifactTable: %v", err)
	}

	rows, err := r.DB().Query("EXPLAIN QUERY PLAN " + candidateQuerySQL)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN returned no rows")
	}

	for _, step := range plan {
		if strings.Contains(step, "CORRELATED") {
			t.Fatalf("candidate query rescans a derived table per blob: %q\nplan: %v", step, plan)
		}
	}
}
