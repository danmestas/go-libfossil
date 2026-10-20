// Package verify provides comprehensive repository verification and rebuild.
package verify

import (
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// IssueKind categorizes the type of verification issue found.
type IssueKind int

const (
	IssueHashMismatch IssueKind = iota
	IssueBlobCorrupt
	IssueDeltaDangling
	IssuePhantomOrphan
	IssueEventMissing
	IssueEventMismatch
	IssueMlinkMissing
	IssuePlinkMissing
	IssueTagxrefMissing
	IssueFilenameMissing
	IssueLeafIncorrect
	IssueMissingReference
)

// Issue represents a single verification problem found in the repository.
type Issue struct {
	Kind    IssueKind
	RID     libfossil.FslID
	UUID    string
	Table   string
	Message string
}

// Report contains the results of a repository verification pass.
type Report struct {
	Issues       []Issue
	BlobsChecked int
	BlobsOK      int
	BlobsFailed  int
	BlobsSkipped int
	MissingRefs  int
	// Phantoms counts the artifacts the store names but lacks and still
	// wants: transit phantoms, which synchronization requests. Shunned and
	// elided artifacts are not among them.
	Phantoms int
	// Elided counts the artifacts deliberately absent under the store cone
	// (draft-fossil-sparse-cone-00 §5.2). Elision is healthy: it is never an
	// Issue, so a store complete within its cone verifies OK, and it is
	// counted apart from damage and from Phantoms so that neither hides the
	// other. It is zero for a dense store.
	Elided        int
	TablesRebuilt []string
	Duration      time.Duration
}

// OK returns true if no issues were found during verification.
func (r *Report) OK() bool {
	return len(r.Issues) == 0
}

// addIssue appends a new issue to the report.
func (r *Report) addIssue(issue Issue) {
	r.Issues = append(r.Issues, issue)
}

// Verify performs comprehensive repository verification.
// It checks blob integrity, delta chains, phantom records, and derived tables,
// and counts what the store lacks by absence class.
// Returns a report of all issues found. Never stops early - reports all problems.
func Verify(r *repo.Repo) (*Report, error) {
	if r == nil {
		panic("verify: nil repo")
	}

	start := time.Now()
	report := &Report{}

	// Phase 1: Blob integrity (content hash verification)
	if err := checkBlobs(r, report); err != nil {
		return nil, err
	}

	// Phase 2: Structural integrity (delta chains, phantoms)
	if err := checkDeltaChains(r, report); err != nil {
		return nil, err
	}
	if err := checkPhantoms(r, report); err != nil {
		return nil, err
	}

	// Phase 3: Derived tables (event, mlink, plink, tagxref, filename, leaf)
	if err := checkDerived(r, report); err != nil {
		return nil, err
	}

	// Phase 4: what the store lacks, by absence class.
	if err := countAbsences(r, report); err != nil {
		return nil, err
	}

	report.Duration = time.Since(start)
	return report, nil
}

// countAbsences fills Phantoms and Elided from the store's phantoms,
// classified against its store cone. It runs after the derived-table checks
// because elision is read from mlink, a derived table.
func countAbsences(r *repo.Repo, report *Report) error {
	if r == nil {
		panic("verify.countAbsences: nil *repo.Repo")
	}
	if report == nil {
		panic("verify.countAbsences: nil *Report")
	}
	a, err := content.LoadAbsences(r.DB())
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	report.Phantoms = a.Count(content.Transit)
	report.Elided = a.Count(content.Elided)
	return nil
}
