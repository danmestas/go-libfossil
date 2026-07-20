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

	return expandChain(q, rid, nil, nil)
}

// expandChain expands rid by walking its delta chain back to a materialized
// starting point and replaying the deltas forward.
//
// have, when non-nil, reports content already materialized for an rid; the
// walk stops at the first ancestor it recognises instead of continuing to
// the chain root. put, when non-nil, receives every node this call
// materialized. Both are nil for a plain [Expand], which then walks and
// replays the whole chain exactly as it always has.
//
// The two hooks are what make repeated expansion of one chain linear rather
// than quadratic. In a real repository content_deltify has rewritten older
// blobs as deltas against newer ones, so a single file's history is one
// chain thousands of nodes deep; expanding every node of it from the root
// costs O(n^2) blob reads, and that is where a large clone's crosslink pass
// spends over 90% of its time.
func expandChain(
	q db.Querier,
	rid libfossil.FslID,
	have func(libfossil.FslID) ([]byte, bool),
	put func(libfossil.FslID, []byte),
) ([]byte, error) {
	chain, base, err := walkDeltaChain(q, rid, have)
	if err != nil {
		return nil, fmt.Errorf("content.Expand: %w", err)
	}
	// The replay below ends holding rid's content, and the final
	// verification names rid; both are wrong if the walk ended elsewhere.
	if chain[len(chain)-1] != rid {
		panic("content.expandChain: chain must end at rid")
	}

	// Every node this call materializes is verified before it is handed to
	// put: a cached interior is served later as if it had been expanded in
	// its own right, so it has to have earned the same guarantee. A plain
	// Expand (put == nil) materializes nothing for anyone else and so still
	// verifies exactly one blob, the one it was asked for.
	//
	// base is the content the walk stopped on, carried out of the walk
	// rather than re-read from have. Re-reading would be a second decision:
	// between the walk truncating at a node the store held and this point,
	// a concurrent eviction could remove it, and chain[0] would then be
	// loaded with blob.Load -- which for a delta row hands raw delta bytes
	// to the replay as if they were full text. bindToName catches that, but
	// the caller sees a hash mismatch on an artifact that is not corrupt,
	// and Crosslink's error path drops such artifacts silently.
	content := base
	if content == nil {
		content, err = blob.Load(q, chain[0])
		if err != nil {
			return nil, fmt.Errorf("content.Expand load root rid=%d: %w", chain[0], err)
		}
		if put != nil {
			if err := bindToName(q, chain[0], content); err != nil {
				return nil, err
			}
			put(chain[0], content)
		}
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
		if put != nil {
			if err := bindToName(q, chain[i], content); err != nil {
				return nil, err
			}
			put(chain[i], content)
		}
	}

	// When put is non-nil every node above was verified on its way into the
	// caller's store, rid included; hashing it a second time here would
	// double the cost of a sweep for nothing.
	verified := put != nil

	// BUGGIFY: flip a byte in expanded content to exercise the hash check
	// below under DST. It fires once per expansion, on the artifact the
	// caller named, whether or not interiors were materialized on the way.
	if simio.Buggify(0.01) && len(content) > 0 {
		corrupted := make([]byte, len(content))
		copy(corrupted, content)
		corrupted[0] ^= 0xFF
		content = corrupted
		verified = false
	}

	if !verified {
		if err := bindToName(q, rid, content); err != nil {
			return nil, err
		}
	}

	return content, nil
}

// bindToName checks expanded content against the UUID its blob row is stored
// under.
//
// A delta's own trailing checksum (see delta.Apply) only proves the bytes are
// self-consistent with what the sender transmitted -- it says nothing about
// whether those bytes match the UUID this row is stored under.
// blob.StoreDeltaRaw persists a delta, and phantomizes its base, before that
// base ever arrives; nothing upstream of this point has checked the claim,
// and nothing notifies this function when a base fills in -- IsAvailable and
// the chain walk are both recomputed live on every call. Verifying here, the
// single choke point every expansion path goes through (Cache.Expand,
// crosslinking, checkout, diff, merge, ...), catches a mismatch regardless of
// which caller reached it or how the chain assembled.
func bindToName(q db.Querier, rid libfossil.FslID, content []byte) error {
	var uuid string
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		return fmt.Errorf("content.Expand: query uuid for rid=%d: %w", rid, err)
	}
	var computed string
	if len(uuid) == 64 {
		computed = hash.SHA3(content)
	} else {
		computed = hash.SHA1(content)
	}
	if computed != uuid {
		return fmt.Errorf("content.Expand: hash mismatch for rid=%d: stored=%s computed=%s", rid, uuid, computed)
	}
	return nil
}

// walkDeltaChain returns the delta chain ending at rid, root first.
//
// have, when non-nil, truncates the walk at the deepest ancestor whose content
// the caller already holds; that ancestor becomes chain[0] in place of the true
// chain root, and its content is returned as base. base is nil when the walk
// reached a real chain root, which the caller must then load itself.
//
// Returning the content, rather than leaving the caller to ask have a second
// time, is what makes the truncation decision and the use of it one step: a
// store that can evict concurrently could otherwise drop the node in between.
func walkDeltaChain(q db.Querier, rid libfossil.FslID, have func(libfossil.FslID) ([]byte, bool)) (chain []libfossil.FslID, base []byte, err error) {
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

	// The visited set stops a cycle at its first repeat; the depth bound is
	// the backstop for a chain that is acyclic but longer than any real one.
	// See maxDeltaChainDepth.
	for depth := 0; depth <= maxDeltaChainDepth; depth++ {
		if seen[current] {
			return nil, nil, fmt.Errorf("delta chain cycle detected at rid=%d", current)
		}
		seen[current] = true
		chain = append(chain, current)

		if have != nil {
			if cached, ok := have(current); ok {
				base = cached
				break
			}
		}

		var sourceID int64
		err := q.QueryRow("SELECT srcid FROM delta WHERE rid=?", current).Scan(&sourceID)
		if err != nil {
			break
		}
		current = libfossil.FslID(sourceID)

		if depth == maxDeltaChainDepth {
			return nil, nil, fmt.Errorf(
				"delta chain from rid=%d exceeds %d nodes", rid, maxDeltaChainDepth)
		}
	}

	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, base, nil
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
