package blob

import (
	"fmt"

	libfossil "github.com/danmestas/libfossil/internal/fsltype"
	"github.com/danmestas/libfossil/db"
	"github.com/danmestas/libfossil/internal/delta"
	"github.com/danmestas/libfossil/internal/hash"
)

func Store(q db.Querier, content []byte) (rid libfossil.FslID, uuid string, err error) {
	if q == nil {
		panic("blob.Store: q must not be nil")
	}
	if len(content) == 0 {
		panic("blob.Store: content length must be > 0")
	}
	defer func() {
		if err == nil && rid <= 0 {
			panic("blob.Store: postcondition violated: rid <= 0 with no error")
		}
	}()

	uuid = hash.SHA1(content)

	if rid, ok := Exists(q, uuid); ok {
		return rid, uuid, nil
	}

	compressed, err := Compress(content)
	if err != nil {
		return 0, "", fmt.Errorf("blob.Store compress: %w", err)
	}

	result, err := q.Exec(
		"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, ?, ?, 1)",
		uuid, len(content), compressed,
	)
	if err != nil {
		return 0, "", fmt.Errorf("blob.Store insert: %w", err)
	}

	ridInt, err := result.LastInsertId()
	if err != nil {
		return 0, "", fmt.Errorf("blob.Store lastid: %w", err)
	}

	rid = libfossil.FslID(ridInt)

	// Verify round-trip: re-read, decompress, re-hash.
	// Matches Fossil's content_put_pk() post-write verification.
	readBack, err := Load(q, rid)
	if err != nil {
		return 0, "", fmt.Errorf("blob.Store verify read-back: %w", err)
	}
	if got := hash.SHA1(readBack); got != uuid {
		return 0, "", fmt.Errorf("blob.Store verify: hash mismatch after round-trip: stored %s, got %s", uuid, got)
	}

	// Mark as unclustered — matches Fossil's content_put_ex (content.c:633).
	// Only new blobs reach here; Exists early-return skips this.
	if _, err := q.Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rid); err != nil {
		return 0, "", fmt.Errorf("blob.Store unclustered: %w", err)
	}

	return rid, uuid, nil
}

func StoreDelta(q db.Querier, content []byte, srcRid libfossil.FslID) (rid libfossil.FslID, uuid string, err error) {
	if q == nil {
		panic("blob.StoreDelta: q must not be nil")
	}
	if len(content) == 0 {
		panic("blob.StoreDelta: content length must be > 0")
	}
	if srcRid <= 0 {
		panic("blob.StoreDelta: srcRid must be > 0")
	}
	defer func() {
		if err == nil && rid <= 0 {
			panic("blob.StoreDelta: postcondition violated: rid <= 0 with no error")
		}
	}()

	uuid = hash.SHA1(content)

	if rid, ok := Exists(q, uuid); ok {
		return rid, uuid, nil
	}

	srcContent, err := Load(q, srcRid)
	if err != nil {
		return 0, "", fmt.Errorf("blob.StoreDelta load source: %w", err)
	}

	deltaBytes := delta.Create(srcContent, content)
	compressed, err := Compress(deltaBytes)
	if err != nil {
		return 0, "", fmt.Errorf("blob.StoreDelta compress: %w", err)
	}

	result, err := q.Exec(
		"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, ?, ?, 1)",
		uuid, len(content), compressed,
	)
	if err != nil {
		return 0, "", fmt.Errorf("blob.StoreDelta insert blob: %w", err)
	}

	ridInt, err := result.LastInsertId()
	if err != nil {
		return 0, "", fmt.Errorf("blob.StoreDelta lastid: %w", err)
	}

	rid = libfossil.FslID(ridInt)
	_, err = q.Exec("INSERT INTO delta(rid, srcid) VALUES(?, ?)", rid, srcRid)
	if err != nil {
		return 0, "", fmt.Errorf("blob.StoreDelta insert delta: %w", err)
	}

	// Verify round-trip: re-read delta, apply to source, re-hash.
	// Matches Fossil's content_put_pk() post-write verification.
	storedDelta, err := Load(q, rid)
	if err != nil {
		return 0, "", fmt.Errorf("blob.StoreDelta verify read-back: %w", err)
	}
	rebuilt, err := delta.Apply(srcContent, storedDelta)
	if err != nil {
		return 0, "", fmt.Errorf("blob.StoreDelta verify apply: %w", err)
	}
	if got := hash.SHA1(rebuilt); got != uuid {
		return 0, "", fmt.Errorf("blob.StoreDelta verify: hash mismatch after round-trip: stored %s, got %s", uuid, got)
	}

	// Mark as unclustered — matches Fossil's content_put_ex (content.c:633).
	if _, err := q.Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rid); err != nil {
		return 0, "", fmt.Errorf("blob.StoreDelta unclustered: %w", err)
	}

	return rid, uuid, nil
}

// StoreDeltaRaw stores delta-encoded content exactly as given — without
// expanding it against its source — and records the delta-to-source link.
// Unlike StoreDelta, which computes a delta from full target content and
// verifies the round-trip against an already-readable source, StoreDeltaRaw
// accepts content that arrived already delta-encoded (e.g. over the wire
// during a transfer) whose source may itself still be a phantom. The
// target's size is read from the delta's own header (delta.OutputSize), so
// no source content is needed to store it.
//
// If a real (non-phantom) blob already exists for uuid, this is a no-op.
// If a phantom row exists, it is filled in place. Otherwise a new row is
// created. In every case the row's declared size is real, never -1: the
// target is not phantomized just because its source might be — mirrors
// Fossil's content_put_ex (src/content.c:557-620), which stores delta
// content unconditionally and records REPLACE INTO delta(rid,srcid)
// whether or not the source is currently available. Availability resolves
// lazily elsewhere (content.IsAvailable, manifest.Crosslink).
func StoreDeltaRaw(q db.Querier, uuid string, deltaBytes []byte, srcRid libfossil.FslID) (rid libfossil.FslID, err error) {
	if q == nil {
		panic("blob.StoreDeltaRaw: q must not be nil")
	}
	if uuid == "" {
		panic("blob.StoreDeltaRaw: uuid must not be empty")
	}
	if len(deltaBytes) == 0 {
		panic("blob.StoreDeltaRaw: deltaBytes must not be empty")
	}
	if srcRid <= 0 {
		panic("blob.StoreDeltaRaw: srcRid must be > 0")
	}
	defer func() {
		if err == nil && rid <= 0 {
			panic("blob.StoreDeltaRaw: postcondition violated: rid <= 0 with no error")
		}
	}()

	existingRid, exists := Exists(q, uuid)
	if exists {
		var size int64
		if err := q.QueryRow("SELECT size FROM blob WHERE rid=?", existingRid).Scan(&size); err != nil {
			return 0, fmt.Errorf("blob.StoreDeltaRaw: check existing: %w", err)
		}
		if size != -1 {
			return existingRid, nil // real blob already exists; nothing to do
		}
	}

	targetSize, err := delta.OutputSize(deltaBytes)
	if err != nil {
		return 0, fmt.Errorf("blob.StoreDeltaRaw: output size: %w", err)
	}
	compressed, err := Compress(deltaBytes)
	if err != nil {
		return 0, fmt.Errorf("blob.StoreDeltaRaw: compress: %w", err)
	}

	if exists {
		// Filling a phantom.
		if _, err := q.Exec("UPDATE blob SET rcvid=1, size=?, content=? WHERE rid=?",
			int64(targetSize), compressed, existingRid); err != nil {
			return 0, fmt.Errorf("blob.StoreDeltaRaw: fill phantom: %w", err)
		}
		if _, err := q.Exec("DELETE FROM phantom WHERE rid=?", existingRid); err != nil {
			return 0, fmt.Errorf("blob.StoreDeltaRaw: clear phantom: %w", err)
		}
		rid = existingRid
	} else {
		result, err := q.Exec(
			"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, ?, ?, 1)",
			uuid, int64(targetSize), compressed,
		)
		if err != nil {
			return 0, fmt.Errorf("blob.StoreDeltaRaw: insert: %w", err)
		}
		ridInt, err := result.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("blob.StoreDeltaRaw: lastid: %w", err)
		}
		rid = libfossil.FslID(ridInt)
	}

	// Record the delta-to-source link unconditionally — matches
	// content_put_ex's REPLACE INTO delta(rid,srcid), which fires
	// regardless of whether srcRid is itself currently available.
	if _, err := q.Exec("REPLACE INTO delta(rid, srcid) VALUES(?, ?)", rid, srcRid); err != nil {
		return 0, fmt.Errorf("blob.StoreDeltaRaw: delta link: %w", err)
	}
	if _, err := q.Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rid); err != nil {
		return 0, fmt.Errorf("blob.StoreDeltaRaw: unclustered: %w", err)
	}

	return rid, nil
}

func StorePhantom(q db.Querier, uuid string) (rid libfossil.FslID, err error) {
	if q == nil {
		panic("blob.StorePhantom: q must not be nil")
	}
	if uuid == "" {
		panic("blob.StorePhantom: uuid must not be empty")
	}
	defer func() {
		if err == nil && rid <= 0 {
			panic("blob.StorePhantom: postcondition violated: rid <= 0 with no error")
		}
	}()

	if rid, ok := Exists(q, uuid); ok {
		return rid, nil
	}

	result, err := q.Exec(
		"INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, -1, NULL, 0)",
		uuid,
	)
	if err != nil {
		return 0, fmt.Errorf("blob.StorePhantom: %w", err)
	}

	ridInt, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("blob.StorePhantom lastid: %w", err)
	}

	rid = libfossil.FslID(ridInt)
	_, err = q.Exec("INSERT INTO phantom(rid) VALUES(?)", rid)
	if err != nil {
		return 0, fmt.Errorf("blob.StorePhantom phantom table: %w", err)
	}

	return rid, nil
}

func Load(q db.Querier, rid libfossil.FslID) (result []byte, err error) {
	if q == nil {
		panic("blob.Load: q must not be nil")
	}
	if rid <= 0 {
		panic("blob.Load: rid must be > 0")
	}
	defer func() {
		if err == nil && result == nil {
			panic("blob.Load: postcondition violated: result is nil with no error")
		}
	}()

	var content []byte
	var size int64
	err = q.QueryRow("SELECT content, size FROM blob WHERE rid=?", rid).Scan(&content, &size)
	if err != nil {
		return nil, fmt.Errorf("blob.Load query: %w", err)
	}

	if size == -1 {
		return nil, fmt.Errorf("blob.Load: rid %d is a phantom", rid)
	}

	if content == nil || len(content) == 0 {
		return nil, fmt.Errorf("blob.Load: rid %d has NULL or empty content", rid)
	}

	// Fossil stores blobs as [4-byte BE uncompressed-size][zlib data].
	// When the compressed form happens to be the same length as the
	// uncompressed content (rare but real — ~2 in 66K in the Fossil SCM
	// repo), we must still decompress. Detect compressed content by
	// checking for the 4-byte BE prefix matching the declared size
	// followed by a zlib header (0x78).
	if len(content) >= 6 {
		prefixSize := int64(content[0])<<24 | int64(content[1])<<16 | int64(content[2])<<8 | int64(content[3])
		if prefixSize == size && content[4] == 0x78 {
			return Decompress(content)
		}
	}
	// No compression prefix — content is stored uncompressed.
	if int64(len(content)) == size {
		return content, nil
	}
	// Stored bytes < declared size — compressed.
	return Decompress(content)
}

// Exists reports whether a blob row with the given uuid is present, and
// returns its rid. It answers existence only.
//
// It returns true for a phantom — a real blob row with size = -1 and NULL
// content, standing in for an artifact we know of but have not received.
// A phantom's content cannot be read, and neither can that of a delta whose
// chain bottoms out in one. Callers that are about to read content must use
// content.AvailableByUUID instead, which is transitive over the delta chain.
//
// Mirrors Fossil's rid_from_uuid (src/xfer.c:70).
func Exists(q db.Querier, uuid string) (libfossil.FslID, bool) {
	if q == nil {
		panic("blob.Exists: q must not be nil")
	}
	if uuid == "" {
		panic("blob.Exists: uuid must not be empty")
	}
	var rid int64
	err := q.QueryRow("SELECT rid FROM blob WHERE uuid=?", uuid).Scan(&rid)
	if err != nil {
		return 0, false
	}
	return libfossil.FslID(rid), true
}
