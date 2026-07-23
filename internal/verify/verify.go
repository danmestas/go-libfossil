// Package verify provides comprehensive repository verification and rebuild.
package verify

import (
	"time"

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
	Issues        []Issue
	BlobsChecked  int
	BlobsOK       int
	BlobsFailed   int
	BlobsSkipped  int
	MissingRefs   int
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
// It checks blob integrity, delta chains, phantom records, and derived tables.
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

	report.Duration = time.Since(start)
	return report, nil
}
