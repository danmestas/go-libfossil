package verify

import (
	"fmt"

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
		rid := libfossil.FslID(e.rid)
		data, err := cache.Expand(r.DB(), rid)
		if err != nil {
			report.BlobsFailed++
			report.addIssue(Issue{
				Kind:    IssueBlobCorrupt,
				RID:     rid,
				UUID:    e.uuid,
				Table:   "blob",
				Message: fmt.Sprintf("rid %d: expand failed: %v", e.rid, err),
			})
			continue
		}
		var computed string
		if len(e.uuid) == 64 {
			computed = hash.SHA3(data)
		} else {
			computed = hash.SHA1(data)
		}
		if computed != e.uuid {
			report.BlobsFailed++
			report.addIssue(Issue{
				Kind:    IssueHashMismatch,
				RID:     rid,
				UUID:    e.uuid,
				Table:   "blob",
				Message: fmt.Sprintf("rid %d: hash mismatch (stored=%s computed=%s)", e.rid, e.uuid, computed),
			})
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
