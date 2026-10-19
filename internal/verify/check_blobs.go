package verify

import (
	"errors"
	"fmt"
	"sort"

	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// checkBlobs verifies blob integrity: content hashing and delta application.
// It expands every non-phantom blob once, in delta-tree order, and checks the
// content against the blob's stored UUID; see content.ExpandAll for why a
// blob-by-blob expansion is quadratic in chain depth on a real repository.
// Issues are reported in rid order.
func checkBlobs(r *repo.Repo, report *Report) error {
	if r == nil {
		panic("checkBlobs: nil *repo.Repo")
	}
	if report == nil {
		panic("checkBlobs: nil *Report")
	}

	var issues []Issue
	err := content.ExpandAll(r.DB(), func(rid libfossil.FslID, uuid string, _ []byte, err error) error {
		report.BlobsChecked++
		if issue := blobIssue(rid, uuid, err); issue != nil {
			report.BlobsFailed++
			issues = append(issues, *issue)
			return nil
		}
		report.BlobsOK++
		return nil
	})
	if err != nil {
		return fmt.Errorf("checkBlobs: %w", err)
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].RID < issues[j].RID })
	for _, issue := range issues {
		report.addIssue(issue)
	}

	// Postcondition: every checked blob is either OK or failed.
	if report.BlobsChecked != report.BlobsOK+report.BlobsFailed {
		panic("checkBlobs: postcondition: BlobsChecked != BlobsOK + BlobsFailed")
	}

	return nil
}

// blobIssue classifies the outcome of expanding one blob, whose content
// content.ExpandAll has already bound to uuid. It returns nil when the blob is
// sound.
func blobIssue(rid libfossil.FslID, uuid string, err error) *Issue {
	if err == nil {
		return nil
	}
	issue := &Issue{RID: rid, UUID: uuid, Table: "blob"}
	var mismatch *content.HashMismatchError
	if errors.As(err, &mismatch) && mismatch.RID != rid {
		// An interior delta base is the misnamed blob; its own row reports
		// the mismatch. This one cannot be expanded because its base failed.
		issue.Kind = IssueBlobCorrupt
		issue.Message = fmt.Sprintf("rid %d: expand failed: base %v", rid, err)
		return issue
	}
	if errors.Is(err, content.ErrHashMismatch) {
		// Expansion binds content to its name, so a mismatch surfaces as
		// this error. The content expanded; reporting it as corrupt would
		// hide that only the name is wrong.
		issue.Kind = IssueHashMismatch
		issue.Message = fmt.Sprintf("rid %d: %v", rid, err)
		return issue
	}
	issue.Kind = IssueBlobCorrupt
	issue.Message = fmt.Sprintf("rid %d: expand failed: %v", rid, err)
	return issue
}
