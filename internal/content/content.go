package content

import (
	"fmt"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/internal/blob"
	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/delta"
	"github.com/danmestas/libfossil/internal/hash"
	"github.com/danmestas/libfossil/simio"
)

func Expand(q db.Querier, rid libfossil.FslID) (result []byte, err error) {
	if q == nil {
		panic("content.Expand: q must not be nil")
	}
	if rid <= 0 {
		panic("content.Expand: rid must be > 0")
	}
	defer func() {
		if err == nil && result == nil {
			panic("content.Expand: postcondition violated: result is nil with no error")
		}
	}()

	chain, err := walkDeltaChain(q, rid)
	if err != nil {
		return nil, fmt.Errorf("content.Expand: %w", err)
	}

	content, err := blob.Load(q, chain[0])
	if err != nil {
		return nil, fmt.Errorf("content.Expand load root rid=%d: %w", chain[0], err)
	}

	for i := 1; i < len(chain); i++ {
		deltaBytes, err := blob.Load(q, chain[i])
		if err != nil {
			return nil, fmt.Errorf("content.Expand load delta rid=%d: %w", chain[i], err)
		}
		content, err = delta.Apply(content, deltaBytes)
		if err != nil {
			return nil, fmt.Errorf("content.Expand apply delta rid=%d: %w", chain[i], err)
		}
	}

	// BUGGIFY: flip a byte in expanded content to exercise the hash check
	// below under DST.
	if simio.Buggify(0.01) && len(content) > 0 {
		corrupted := make([]byte, len(content))
		copy(corrupted, content)
		corrupted[0] ^= 0xFF
		content = corrupted
	}

	// Bind content to name. A delta's own trailing checksum (see
	// delta.Apply) only proves the bytes are self-consistent with what the
	// sender transmitted -- it says nothing about whether those bytes
	// match the UUID this row is stored under. blob.StoreDeltaRaw persists
	// a delta, and phantomizes its base, before that base ever arrives;
	// nothing upstream of this point has checked the claim, and nothing
	// notifies this function when a base fills in -- IsAvailable and this
	// walk are both recomputed live on every call. Verifying here, the
	// single choke point every expansion path goes through (Cache.Expand,
	// crosslinking, checkout, diff, merge, ...), catches a mismatch
	// regardless of which caller reached it or how the chain assembled.
	var uuid string
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		return nil, fmt.Errorf("content.Expand: query uuid for rid=%d: %w", rid, err)
	}
	var computed string
	if len(uuid) == 64 {
		computed = hash.SHA3(content)
	} else {
		computed = hash.SHA1(content)
	}
	if computed != uuid {
		return nil, fmt.Errorf("content.Expand: hash mismatch for rid=%d: stored=%s computed=%s", rid, uuid, computed)
	}

	return content, nil
}

func walkDeltaChain(q db.Querier, rid libfossil.FslID) (chain []libfossil.FslID, err error) {
	if q == nil {
		panic("content.walkDeltaChain: q must not be nil")
	}
	if rid <= 0 {
		panic("content.walkDeltaChain: rid must be > 0")
	}
	defer func() {
		if err == nil && len(chain) == 0 {
			panic("content.walkDeltaChain: postcondition violated: chain is empty with no error")
		}
	}()

	current := rid
	seen := make(map[libfossil.FslID]bool)

	for {
		if seen[current] {
			return nil, fmt.Errorf("delta chain cycle detected at rid=%d", current)
		}
		seen[current] = true
		chain = append(chain, current)

		var sourceID int64
		err := q.QueryRow("SELECT srcid FROM delta WHERE rid=?", current).Scan(&sourceID)
		if err != nil {
			break
		}
		current = libfossil.FslID(sourceID)
	}

	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// Verify confirms that rid's stored content hashes to its declared UUID.
// Expand performs this same check internally on every expansion now, so
// Verify is a thin, explicitly-named wrapper for callers (repo rebuild,
// integration tests) that want to state their intent as verification
// rather than retrieval.
func Verify(q db.Querier, rid libfossil.FslID) error {
	if q == nil {
		panic("content.Verify: q must not be nil")
	}
	if rid <= 0 {
		panic("content.Verify: rid must be > 0")
	}
	if _, err := Expand(q, rid); err != nil {
		return fmt.Errorf("content.Verify: %w", err)
	}
	return nil
}

func IsPhantom(q db.Querier, rid libfossil.FslID) (bool, error) {
	if q == nil {
		panic("content.IsPhantom: q must not be nil")
	}
	if rid <= 0 {
		panic("content.IsPhantom: rid must be > 0")
	}

	var count int
	err := q.QueryRow("SELECT count(*) FROM phantom WHERE rid=?", rid).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
