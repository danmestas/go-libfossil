package content

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/internal/delta"
	libfossil "github.com/danmestas/libfossil/internal/fsltype"
)

// Deltification policy, ported from canonical Fossil's content_deltify()
// (fossil-2.28 src/content.c:852-947). Every rule below is canonical's; this
// package is the only place any of them is stated, so the commit path and the
// crosslink path cannot drift apart on the question of what gets deltified.
//
// The shape of the policy is easy to get backwards. Fossil deltifies
// BACKWARDS: a newly created artifact is always stored whole, and its
// PREDECESSOR is then rewritten as a delta against it. Callers therefore pass
// the OLD rid as rid and the NEW one as srcRid -- see checkin.c:3133
// (content_deltify(rid, &nrid, ...) after content_put of the new file
// content) and manifest.c:1562 (content_deltify(pid, &fid, ...) in
// add_mlink). The consequence is that the tip of every delta chain is full
// content, so reading the newest version of a file is always O(1) blobs, and
// an artifact's chain depth is its age in revisions.
//
// Chain depth is bounded by deltifyMinRatio and by the never-redeltify rule:
// an artifact that is already a delta is left alone (content.c:869), so each
// artifact is converted at most once and depth grows by one per subsequent
// revision of the same file rather than compounding. Canonical caps depth no
// further at store time; `fossil rebuild` re-runs the pass offline
// (rebuild.c:533).
const (
	// Artifacts below this size are left whole: the delta header and
	// checksum would cost more than the copy instructions save.
	// content.c:881 ("Do not try to create a delta for objects smaller
	// than 50 bytes") and content.c:911 for the source side.
	deltifyMinBytes = 50

	// A delta must be smaller than this fraction of the target content to
	// be worth the extra indirection on every read. content.c:917
	// (blob_size(&delta) < blob_size(&data)*0.75).
	deltifyMinRatio = 0.75
)

// Deltify tries to rewrite the artifact rid, currently stored whole, as a
// delta against srcRid. It returns the number of stored bytes saved; 0 means
// the policy above declined and rid was left untouched, which is a normal
// outcome and not an error.
//
// The rewrite changes only how rid is stored, never what it expands to, so
// callers need not tell anyone the representation changed.
func Deltify(q db.Querier, rid, srcRid libfossil.FslID) (saved int, err error) {
	if q == nil {
		panic("content.Deltify: q must not be nil")
	}
	defer func() {
		if err == nil && saved < 0 {
			panic("content.Deltify: postcondition violated: negative saving with no error")
		}
	}()

	// Canonical tolerates rid 0 rather than making every call site check
	// (content.c:864). Callers pass "the previous version", which legitimately
	// does not exist for a file's first revision.
	if rid <= 0 || srcRid <= 0 || rid == srcRid {
		return 0, nil
	}

	ok, err := deltifyEligible(q, rid, srcRid)
	if err != nil || !ok {
		return 0, err
	}

	data, err := Expand(q, rid)
	if err != nil {
		return 0, fmt.Errorf("content.Deltify: expand rid=%d: %w", rid, err)
	}
	if len(data) < deltifyMinBytes {
		return 0, nil
	}
	src, err := Expand(q, srcRid)
	if err != nil {
		return 0, fmt.Errorf("content.Deltify: expand srcid=%d: %w", srcRid, err)
	}
	if len(src) < deltifyMinBytes {
		return 0, nil
	}

	deltaBytes := delta.Create(src, data)
	if float64(len(deltaBytes)) >= float64(len(data))*deltifyMinRatio {
		return 0, nil
	}
	return deltifyWrite(q, rid, srcRid, deltaBytes)
}

// deltifyEligible answers the questions that can be settled without reading
// content: is rid already a delta, is it real, and would the link leak a
// private artifact into a public one.
func deltifyEligible(q db.Querier, rid, srcRid libfossil.FslID) (bool, error) {
	// Already a delta: leave it. This is the rule that keeps chain depth
	// linear in revision count instead of compounding (content.c:869).
	source, err := deltaSource(q, rid)
	if err != nil {
		return false, err
	}
	if source > 0 {
		return false, nil
	}

	// Phantoms have no content to delta (content.c:874).
	if !IsAvailable(q, rid) || !IsAvailable(q, srcRid) {
		return false, nil
	}

	// Never carry a private artifact into a public one: the far side of a
	// sync would receive the public artifact but never be allowed the
	// source it deltas against (content.c:832-836).
	if IsPrivate(q, int64(srcRid)) && !IsPrivate(q, int64(rid)) {
		return false, nil
	}

	// If srcRid already depends on rid, making rid depend on srcRid would
	// close a loop. Canonical undeltas srcRid to break the existing
	// dependency and then still declines this pairing for now: after the
	// `break`, its loop variable holds rid rather than 0, so the `if( s!=0 )
	// continue` on content.c:907 skips the candidate. Deltifying rid against
	// the now-whole srcRid would be safe, but diverging here would make our
	// storage layout differ from canonical's for the same input, so we
	// decline too. A later revision offers the pair again.
	for cur := srcRid; ; {
		next, err := deltaSource(q, cur)
		if err != nil {
			return false, err
		}
		if next <= 0 {
			return true, nil
		}
		if next == rid {
			return false, Undelta(q, srcRid)
		}
		cur = next
	}
}

func deltifyWrite(q db.Querier, rid, srcRid libfossil.FslID, deltaBytes []byte) (int, error) {
	var before int
	if err := q.QueryRow("SELECT length(content) FROM blob WHERE rid=?", rid).Scan(&before); err != nil {
		return 0, fmt.Errorf("content.Deltify: size rid=%d: %w", rid, err)
	}
	compressed, err := blob.Compress(deltaBytes)
	if err != nil {
		return 0, fmt.Errorf("content.Deltify: compress: %w", err)
	}

	// blob.size stays the target's uncompressed length: it describes what
	// the artifact expands to, not how many bytes the row holds.
	if _, err := q.Exec("UPDATE blob SET content=? WHERE rid=?", compressed, rid); err != nil {
		return 0, fmt.Errorf("content.Deltify: update rid=%d: %w", rid, err)
	}
	if _, err := q.Exec("REPLACE INTO delta(rid, srcid) VALUES(?, ?)", rid, srcRid); err != nil {
		return 0, fmt.Errorf("content.Deltify: link rid=%d: %w", rid, err)
	}

	// Canonical calls verify_before_commit here (content.c:940). Expand
	// re-hashes the expanded result against the row's declared UUID, so a
	// bad delta cannot survive this call.
	if _, err := Expand(q, rid); err != nil {
		return 0, fmt.Errorf("content.Deltify: verify rid=%d: %w", rid, err)
	}

	saved := before - len(compressed)
	if saved < 0 {
		saved = 0
	}
	return saved, nil
}

// Undelta rewrites rid as full content, dropping its delta link. Mirrors
// content_undelta (src/content.c:745-769).
func Undelta(q db.Querier, rid libfossil.FslID) error {
	if q == nil {
		panic("content.Undelta: q must not be nil")
	}
	if rid <= 0 {
		panic("content.Undelta: rid must be > 0")
	}

	source, err := deltaSource(q, rid)
	if err != nil {
		return err
	}
	if source <= 0 {
		return nil
	}

	full, err := Expand(q, rid)
	if err != nil {
		return fmt.Errorf("content.Undelta: expand rid=%d: %w", rid, err)
	}
	compressed, err := blob.Compress(full)
	if err != nil {
		return fmt.Errorf("content.Undelta: compress rid=%d: %w", rid, err)
	}
	if _, err := q.Exec("UPDATE blob SET content=?, size=? WHERE rid=?",
		compressed, len(full), rid); err != nil {
		return fmt.Errorf("content.Undelta: update rid=%d: %w", rid, err)
	}
	if _, err := q.Exec("DELETE FROM delta WHERE rid=?", rid); err != nil {
		return fmt.Errorf("content.Undelta: unlink rid=%d: %w", rid, err)
	}
	return nil
}

// deltaSource returns the rid that rid is stored as a delta against, or 0 if
// rid holds full content.
func deltaSource(q db.Querier, rid libfossil.FslID) (libfossil.FslID, error) {
	var srcid int64
	err := q.QueryRow("SELECT srcid FROM delta WHERE rid=?", rid).Scan(&srcid)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("content.deltaSource rid=%d: %w", rid, err)
	}
	return libfossil.FslID(srcid), nil
}
