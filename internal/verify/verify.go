// Package verify provides comprehensive repository verification and rebuild.
package verify

import (
	"time"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// verifyCacheBytes bounds the expanded content one Verify or Rebuild pass keeps
// live. Both passes sweep every blob more than once -- Verify expands each blob
// in checkBlobs and again in checkCheckins; Rebuild expands each in checkBlobs,
// rebuildManifests, and twice in rebuildTags -- so a single cache shared across
// the pass walks each blob's delta chain once instead of once per sweep, and
// amortizes overlapping chains within each sweep. A miss costs throughput, not
// correctness; blob content is immutable and neither pass rewrites it, so no
// cached entry can go stale. Matches manifest.crosslinkCacheBytes.
const verifyCacheBytes = 256 << 20

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

	// One expansion cache for the whole pass; see verifyCacheBytes.
	cache := content.NewCache(verifyCacheBytes)

	// Phase 1: Blob integrity (content hash verification)
	if err := checkBlobs(r, report, cache); err != nil {
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
	if err := checkDerived(r, report, cache); err != nil {
		return nil, err
	}

	report.Duration = time.Since(start)
	return report, nil
}
