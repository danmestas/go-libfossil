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
//
// Deltify takes a *db.Tx rather than a db.Querier because rewriting an
// artifact is two statements -- the blob content and the delta link -- that
// must land together. A blob holding delta bytes with no delta row expands as
// though it were full content, which is silent corruption rather than a
// detectable error. Canonical wraps the same pair in db_begin_transaction /
// db_end_transaction (content.c:938-941); requiring the transaction in the
// signature makes that non-negotiable at compile time.
func Deltify(tx *db.Tx, rid, srcRid libfossil.FslID) (saved int, err error) {
	if tx == nil {
		panic("content.Deltify: tx must not be nil")
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

	// Already a delta: leave it. This is the rule that keeps chain depth
	// linear in revision count instead of compounding (content.c:869).
	source, err := deltaSource(tx, rid)
	if err != nil {
		return 0, err
	}
	if source > 0 {
		return 0, nil
	}

	// Phantoms have no content to delta (content.c:874).
	//
	// The srcRid half of this check is OURS, not canonical's: content.c
	// guards only content_is_available(rid) and then calls
	// content_get(srcid) unguarded. It is load-bearing beyond phantoms.
	// IsAvailable follows the same delta.srcid links as the ancestor walk
	// below, under maxDeltaChainDepth, and returns false on a cycle -- so a
	// cyclic srcRid is declined here and never reaches deltifyBreaksLoop.
	// Weakening or reordering this guard exposes that walk to a delta graph a
	// sync peer can shape; it carries its own visited set and its own step
	// cap so that exposure is survivable rather than a hang.
	if !IsAvailable(tx, rid) || !IsAvailable(tx, srcRid) {
		return 0, nil
	}

	// The target-size check comes before anything destructive, matching
	// canonical: content.c:877-885 returns on a sub-50-byte target *before*
	// entering the candidate loop, so it never reaches the content_undelta
	// on :902. Running the ancestor walk first would inflate srcRid to full
	// content and then decline anyway -- a pure storage regression, and a
	// divergence from canonical's layout for identical input.
	data, err := Expand(tx, rid)
	if err != nil {
		return 0, fmt.Errorf("content.Deltify: expand rid=%d: %w", rid, err)
	}
	if len(data) < deltifyMinBytes {
		return 0, nil
	}

	// Never carry a private artifact into a public one: the far side of a
	// sync would receive the public artifact but never be allowed the
	// source it deltas against (content.c:895).
	if IsPrivate(tx, int64(srcRid)) && !IsPrivate(tx, int64(rid)) {
		return 0, nil
	}

	loops, err := deltifyBreaksLoop(tx, rid, srcRid)
	if err != nil || loops {
		return 0, err
	}

	src, err := Expand(tx, srcRid)
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
	return deltifyWrite(tx, rid, srcRid, deltaBytes)
}

// deltifyBreaksLoop reports whether rid is already an ancestor of srcRid, in
// which case storing rid as a delta of srcRid would close a loop. When it is,
// srcRid is undeltaed to break the existing dependency and true is returned:
// canonical undeltas and then still declines this pairing, because after its
// `break` the loop variable holds rid rather than 0, so `if( s!=0 ) continue`
// on content.c:907 skips the candidate. Deltifying rid against the now-whole
// srcRid would be safe, but diverging here would make our storage layout
// differ from canonical's for the same input. A later revision offers the
// pair again.
//
// Before reordering Deltify's prologue: this walk is currently unreachable
// with a cyclic srcRid, because the IsAvailable(srcRid) guard above screens
// one out first. That is why the seen-set below never fires in practice, and
// it is not an argument for removing it -- IsAvailable is answering
// groundedness, not loop safety, and the delta graph is peer-shaped
// (blob.StoreDeltaRaw records links from the wire without a cycle check).
// The ordering is asserted, not just documented:
// TestDeltifyDeclinesUngroundedSource forges A->B->A and pins that Deltify
// declines it with no error, so a reorder that exposes this walk turns that
// decline into an error and fails the test.
func deltifyBreaksLoop(tx *db.Tx, rid, srcRid libfossil.FslID) (bool, error) {
	seen := make(map[libfossil.FslID]bool)
	cur := srcRid
	for steps := 0; ; steps++ {
		// Two independent bounds, the same pairing the read-path walks
		// use. The seen set catches a cycle that does not pass through rid
		// -- StoreDeltaRaw can record one from the wire -- and the step cap
		// catches any walk that escapes it. The cap is maxDeltaChainDepth,
		// shared with the read-path walks; see the note on that constant.
		if seen[cur] {
			return false, fmt.Errorf("content.Deltify: delta chain cycle detected at rid=%d", cur)
		}
		if steps > maxDeltaChainDepth {
			return false, fmt.Errorf("content.Deltify: delta chain from rid=%d exceeds %d links", srcRid, maxDeltaChainDepth)
		}
		seen[cur] = true

		next, err := deltaSource(tx, cur)
		if err != nil {
			return false, err
		}
		if next <= 0 {
			return false, nil
		}
		if next == rid {
			return true, Undelta(tx, srcRid)
		}
		cur = next
	}
}

func deltifyWrite(tx *db.Tx, rid, srcRid libfossil.FslID, deltaBytes []byte) (int, error) {
	var before int
	if err := tx.QueryRow("SELECT length(content) FROM blob WHERE rid=?", rid).Scan(&before); err != nil {
		return 0, fmt.Errorf("content.Deltify: size rid=%d: %w", rid, err)
	}
	compressed, err := blob.Compress(deltaBytes)
	if err != nil {
		return 0, fmt.Errorf("content.Deltify: compress: %w", err)
	}

	// blob.size stays the target's uncompressed length: it describes what
	// the artifact expands to, not how many bytes the row holds. Canonical
	// deliberately leaves size alone here (content.c:935) even though
	// content_undelta does update it (content.c:761).
	if _, err := tx.Exec("UPDATE blob SET content=? WHERE rid=?", compressed, rid); err != nil {
		return 0, fmt.Errorf("content.Deltify: update rid=%d: %w", rid, err)
	}
	if _, err := tx.Exec("REPLACE INTO delta(rid, srcid) VALUES(?, ?)", rid, srcRid); err != nil {
		return 0, fmt.Errorf("content.Deltify: link rid=%d: %w", rid, err)
	}

	// Canonical calls verify_before_commit here (content.c:940). Expand
	// re-hashes the expanded result against the row's declared UUID, so a
	// bad delta cannot survive this call.
	if _, err := Expand(tx, rid); err != nil {
		return 0, fmt.Errorf("content.Deltify: verify rid=%d: %w", rid, err)
	}

	saved := before - len(compressed)
	if saved < 0 {
		saved = 0
	}
	return saved, nil
}

// Undelta rewrites rid as full content, dropping its delta link. Mirrors
// content_undelta (src/content.c:745-769), including its update of blob.size,
// which content_deltify deliberately does not touch.
func Undelta(tx *db.Tx, rid libfossil.FslID) error {
	if tx == nil {
		panic("content.Undelta: tx must not be nil")
	}
	if rid <= 0 {
		panic("content.Undelta: rid must be > 0")
	}

	source, err := deltaSource(tx, rid)
	if err != nil {
		return err
	}
	if source <= 0 {
		return nil
	}

	full, err := Expand(tx, rid)
	if err != nil {
		return fmt.Errorf("content.Undelta: expand rid=%d: %w", rid, err)
	}
	compressed, err := blob.Compress(full)
	if err != nil {
		return fmt.Errorf("content.Undelta: compress rid=%d: %w", rid, err)
	}
	if _, err := tx.Exec("UPDATE blob SET content=?, size=? WHERE rid=?",
		compressed, len(full), rid); err != nil {
		return fmt.Errorf("content.Undelta: update rid=%d: %w", rid, err)
	}
	if _, err := tx.Exec("DELETE FROM delta WHERE rid=?", rid); err != nil {
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
