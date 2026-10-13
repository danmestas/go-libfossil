package verify

import (
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// checkBlobs verifies blob integrity: content hashing and delta application.
// Walks every non-phantom blob, expands delta chains, hashes content, and
// compares against stored UUID.
//
// cache is the shared expansion cache for the whole Verify/Rebuild pass. This
// sweep touches every blob, and content_deltify stores older revisions as
// deltas against newer ones, so two blobs in one file's history share the
// same chain: expanding the deepest member materializes and caches every
// interior, turning what would be O(revisions^2) blob reads into O(revisions).
// The same cache carries forward to the later sweeps, which would otherwise
// re-expand every blob from scratch. A nil cache expands uncached.
func checkBlobs(r *repo.Repo, report *Report, cache *content.Cache) error {
	if r == nil {
		panic("checkBlobs: nil *repo.Repo")
	}
	if report == nil {
		panic("checkBlobs: nil *Report")
	}

	rows, err := r.DB().Query("SELECT rid, uuid FROM blob WHERE size >= 0")
	if err != nil {
		return fmt.Errorf("checkBlobs: query: %w", err)
	}
	defer rows.Close()

	type blobEntry struct {
		rid  int64
		uuid string
	}
	var entries []blobEntry
	for rows.Next() {
		var e blobEntry
		if err := rows.Scan(&e.rid, &e.uuid); err != nil {
			return fmt.Errorf("checkBlobs: scan: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("checkBlobs: rows: %w", err)
	}

	for _, e := range entries {
		report.BlobsChecked++
		if issue := checkBlob(r.DB(), cache, libfossil.FslID(e.rid), e.uuid); issue != nil {
			report.BlobsFailed++
			report.addIssue(*issue)
			continue
		}
		report.BlobsOK++
	}

	// Postcondition: every checked blob is either OK or failed.
	if report.BlobsChecked != report.BlobsOK+report.BlobsFailed {
		panic("checkBlobs: postcondition: BlobsChecked != BlobsOK + BlobsFailed")
	}

	return nil
}

// checkBlob expands one blob and hashes it against uuid, its stored name. It
// returns the issue found, or nil when the blob is sound.
func checkBlob(q db.Querier, cache *content.Cache, rid libfossil.FslID, uuid string) *Issue {
	issue := &Issue{RID: rid, UUID: uuid, Table: "blob"}
	data, err := cache.Expand(q, rid)
	var mismatch *content.HashMismatchError
	if errors.As(err, &mismatch) && mismatch.RID != rid {
		// An interior delta base is the misnamed blob; its own row reports
		// the mismatch. This one cannot be expanded because its base failed.
		issue.Kind = IssueBlobCorrupt
		issue.Message = fmt.Sprintf("rid %d: expand failed: base %v", rid, err)
		return issue
	}
	if errors.Is(err, content.ErrHashMismatch) {
		// Expand binds content to its name, so a mismatch surfaces here,
		// not at the comparison below. The content expanded; reporting it
		// as corrupt would hide that only the name is wrong.
		issue.Kind = IssueHashMismatch
		issue.Message = fmt.Sprintf("rid %d: %v", rid, err)
		return issue
	}
	if err != nil {
		issue.Kind = IssueBlobCorrupt
		issue.Message = fmt.Sprintf("rid %d: expand failed: %v", rid, err)
		return issue
	}
	computed := hash.SHA1(data)
	if len(uuid) == 64 {
		computed = hash.SHA3(data)
	}
	if computed != uuid {
		issue.Kind = IssueHashMismatch
		issue.Message = fmt.Sprintf("rid %d: hash mismatch (stored=%s computed=%s)",
			rid, uuid, computed)
		return issue
	}
	return nil
}
