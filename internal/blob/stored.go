package blob

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// A row's stored value is the on-disk blob format: Compress's output, or
// wire bytes already in that format. It normally lives in blob.content.
// When the backend caps value size (db.DriverConfig.MaxValueBytes) and the
// value exceeds the cap, blob.content is NULL and the value is split, in
// order, across blobpart rows keyed by (rid, seq). size stays the
// artifact's expanded length, so a chunked row is never mistaken for a
// phantom (size < 0). Chunking is purely physical: it sits below
// compression, so hashes, deltas and the wire format are unchanged.
//
// Everything that reads or writes blob.content goes through this file.

// PartBytes is the size of every part of a chunked value but the last.
const PartBytes = 1_000_000

// maxParts bounds a chunked value at about 4 GB, past what the 4-byte size
// prefix of the blob format can describe.
const maxParts = 4096

// ErrNotStored reports a row with no stored value: a phantom, or a NULL
// content with no parts.
var ErrNotStored = errors.New("blob: no stored content")

// partsDDL is created on first use, so a store that never chunks keeps
// stock fossil's schema exactly.
const partsDDL = `CREATE TABLE IF NOT EXISTS blobpart(
  rid INTEGER NOT NULL,
  seq INTEGER NOT NULL,
  content BLOB NOT NULL,
  PRIMARY KEY(rid, seq)
)`

// limitOverride holds a test's limit plus one; zero means none.
var limitOverride atomic.Int64

// SetMaxValueBytes overrides the registered driver's MaxValueBytes for this
// process and returns a function restoring the previous setting. It exists
// for tests, which cannot re-register a driver.
func SetMaxValueBytes(n int) (restore func()) {
	if n < 0 || (n > 0 && n < PartBytes) {
		panic("blob.SetMaxValueBytes: n must be 0 or at least PartBytes")
	}
	prev := limitOverride.Swap(int64(n) + 1)
	return func() { limitOverride.Store(prev) }
}

func maxValueBytes() int {
	if o := limitOverride.Load(); o != 0 {
		return int(o - 1)
	}
	if cfg := db.RegisteredDriver(); cfg != nil {
		return cfg.MaxValueBytes
	}
	return 0
}

// chunked reports whether stored must be split under the current limit.
func chunked(stored []byte) bool {
	limit := maxValueBytes()
	if limit > 0 && limit < PartBytes {
		panic("blob: MaxValueBytes below PartBytes")
	}
	return limit > 0 && len(stored) > limit
}

func partsExist(q db.Querier) (bool, error) {
	var n int
	err := q.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='blobpart'").Scan(&n)
	if err != nil {
		return false, fmt.Errorf("blobpart schema: %w", err)
	}
	return n == 1, nil
}

func ensureParts(q db.Querier) error {
	if _, err := q.Exec(partsDDL); err != nil {
		return fmt.Errorf("create blobpart: %w", err)
	}
	return nil
}

// writeParts replaces rid's parts with stored, split into PartBytes pieces.
func writeParts(q db.Querier, rid libfossil.FslID, stored []byte) error {
	n := (len(stored) + PartBytes - 1) / PartBytes
	if n < 2 || n > maxParts {
		panic("blob.writeParts: part count out of range")
	}
	if err := ensureParts(q); err != nil {
		return err
	}
	if _, err := q.Exec("DELETE FROM blobpart WHERE rid=?", rid); err != nil {
		return fmt.Errorf("clear parts rid=%d: %w", rid, err)
	}
	for seq := 0; seq < n; seq++ {
		part := stored[seq*PartBytes : min((seq+1)*PartBytes, len(stored))]
		if _, err := q.Exec("INSERT INTO blobpart(rid, seq, content) VALUES(?, ?, ?)",
			rid, seq, part); err != nil {
			return fmt.Errorf("write part rid=%d seq=%d: %w", rid, seq, err)
		}
	}
	return nil
}

// dropParts deletes rid's parts, if the store has ever chunked.
func dropParts(q db.Querier, rid libfossil.FslID) error {
	ok, err := partsExist(q)
	if err != nil || !ok {
		return err
	}
	if _, err := q.Exec("DELETE FROM blobpart WHERE rid=?", rid); err != nil {
		return fmt.Errorf("drop parts rid=%d: %w", rid, err)
	}
	return nil
}

// WriteStored replaces the stored value of the existing row rid, leaving
// its size, uuid and delta link to the caller.
func WriteStored(q db.Querier, rid libfossil.FslID, stored []byte) error {
	if q == nil {
		panic("blob.WriteStored: q must not be nil")
	}
	if rid <= 0 || len(stored) == 0 {
		panic("blob.WriteStored: rid must be > 0 and stored non-empty")
	}
	var inline any = stored
	if chunked(stored) {
		inline = nil
	}
	res, err := q.Exec("UPDATE blob SET content=? WHERE rid=?", inline, rid)
	if err != nil {
		return fmt.Errorf("blob.WriteStored rid=%d: %w", rid, err)
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("blob.WriteStored: no row rid=%d", rid)
	}
	if inline == nil {
		return writeParts(q, rid, stored)
	}
	return dropParts(q, rid)
}

// InsertStored inserts a new row for uuid holding stored, whose artifact
// expands to size bytes, and returns its rid.
func InsertStored(q db.Querier, uuid string, size int64, stored []byte, rcvid int64) (libfossil.FslID, error) {
	if q == nil || uuid == "" {
		panic("blob.InsertStored: q and uuid must be set")
	}
	if size < 0 || len(stored) == 0 {
		panic("blob.InsertStored: size must be >= 0 and stored non-empty")
	}
	var inline any = stored
	if chunked(stored) {
		inline = nil
	}
	res, err := q.Exec("INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, ?, ?, ?)",
		uuid, size, inline, rcvid)
	if err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("lastid: %w", err)
	}
	rid := libfossil.FslID(id)
	if inline == nil {
		// A rid can be reused after its row was deleted; writeParts clears
		// any parts left under it first.
		return rid, writeParts(q, rid, stored)
	}
	return rid, nil
}

// DropStored turns rid into a phantom: size -1, no content, no parts. The
// caller records it in the phantom table if it wants it requested.
func DropStored(q db.Querier, rid libfossil.FslID) error {
	if q == nil || rid <= 0 {
		panic("blob.DropStored: q must be set and rid > 0")
	}
	if _, err := q.Exec("UPDATE blob SET size=-1, content=NULL WHERE rid=?", rid); err != nil {
		return fmt.Errorf("blob.DropStored rid=%d: %w", rid, err)
	}
	return dropParts(q, rid)
}

// SweepParts deletes every part whose row is gone or no longer chunked. Run
// it after deleting blob rows in bulk.
func SweepParts(q db.Querier) error {
	if q == nil {
		panic("blob.SweepParts: q must not be nil")
	}
	ok, err := partsExist(q)
	if err != nil || !ok {
		return err
	}
	_, err = q.Exec(`DELETE FROM blobpart WHERE rid NOT IN
		(SELECT rid FROM blob WHERE typeof(content)='null' AND size>=0)`)
	if err != nil {
		return fmt.Errorf("blob.SweepParts: %w", err)
	}
	return nil
}

// ReadStored returns rid's stored value: the on-disk blob format, of the
// full artifact or of its delta. It returns ErrNotStored for a phantom.
func ReadStored(q db.Querier, rid libfossil.FslID) ([]byte, error) {
	if q == nil || rid <= 0 {
		panic("blob.ReadStored: q must be set and rid > 0")
	}
	return readStored(q, rid)
}

func readStored(q db.Querier, rid libfossil.FslID) ([]byte, error) {
	var stored []byte
	var size int64
	if err := q.QueryRow("SELECT content, size FROM blob WHERE rid=?", rid).
		Scan(&stored, &size); err != nil {
		return nil, fmt.Errorf("blob: rid %d: %w", rid, err)
	}
	if size < 0 {
		return nil, fmt.Errorf("blob: rid %d is a phantom: %w", rid, ErrNotStored)
	}
	if len(stored) > 0 {
		return stored, nil
	}
	return readParts(q, rid)
}

// readParts joins rid's parts, sizing the result once so a large value is
// never copied while it grows.
func readParts(q db.Querier, rid libfossil.FslID) ([]byte, error) {
	ok, err := partsExist(q)
	if err != nil {
		return nil, err
	}
	var count, total int64
	if ok {
		err = q.QueryRow("SELECT count(*), coalesce(sum(length(content)), 0) FROM blobpart WHERE rid=?",
			rid).Scan(&count, &total)
		if err != nil {
			return nil, fmt.Errorf("blob: parts of rid %d: %w", rid, err)
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("blob: rid %d has NULL or empty content: %w", rid, ErrNotStored)
	}
	if count > maxParts {
		return nil, fmt.Errorf("blob: rid %d has %d parts, over %d", rid, count, maxParts)
	}
	rows, err := q.Query("SELECT seq, content FROM blobpart WHERE rid=? ORDER BY seq", rid)
	if err != nil {
		return nil, fmt.Errorf("blob: parts of rid %d: %w", rid, err)
	}
	defer rows.Close()
	out := make([]byte, 0, total)
	for seq := int64(0); rows.Next(); seq++ {
		var got int64
		var part []byte
		if err := rows.Scan(&got, &part); err != nil {
			return nil, fmt.Errorf("blob: part of rid %d: %w", rid, err)
		}
		if got != seq || int64(len(out)+len(part)) > total {
			return nil, fmt.Errorf("blob: rid %d: parts out of sequence at %d", rid, seq)
		}
		out = append(out, part...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blob: parts of rid %d: %w", rid, err)
	}
	if int64(len(out)) != total {
		return nil, fmt.Errorf("blob: rid %d: parts changed while read", rid)
	}
	return out, nil
}

// StoredHead returns the length of rid's stored value and its first n bytes
// (fewer when the value is shorter), without reading the whole value. It
// returns ErrNotStored for a phantom or a row with no content.
func StoredHead(q db.Querier, rid libfossil.FslID, n int) (int64, []byte, error) {
	if q == nil || rid <= 0 {
		panic("blob.StoredHead: q must be set and rid > 0")
	}
	if n < 0 || n > PartBytes {
		panic("blob.StoredHead: n out of range")
	}
	var size int64
	var length *int64
	var head []byte
	err := q.QueryRow("SELECT size, length(content), substr(content, 1, ?) FROM blob WHERE rid=?",
		n, rid).Scan(&size, &length, &head)
	if err != nil {
		return 0, nil, fmt.Errorf("blob: rid %d: %w", rid, err)
	}
	if size < 0 {
		return 0, nil, fmt.Errorf("blob: rid %d is a phantom: %w", rid, ErrNotStored)
	}
	if length != nil && *length > 0 {
		return *length, head, nil
	}
	return partsHead(q, rid, n)
}

func partsHead(q db.Querier, rid libfossil.FslID, n int) (int64, []byte, error) {
	ok, err := partsExist(q)
	if err != nil {
		return 0, nil, err
	}
	var total int64
	var head []byte
	if ok {
		err = q.QueryRow(`SELECT coalesce(sum(length(content)), 0),
			(SELECT substr(content, 1, ?2) FROM blobpart WHERE rid=?1 AND seq=0)
			FROM blobpart WHERE rid=?1`, rid, n).Scan(&total, &head)
		if err != nil {
			return 0, nil, fmt.Errorf("blob: parts of rid %d: %w", rid, err)
		}
	}
	if total == 0 {
		return 0, nil, fmt.Errorf("blob: rid %d has NULL or empty content: %w", rid, ErrNotStored)
	}
	return total, head, nil
}

// StoredLenSQL returns an SQL expression for the length of the stored
// value of the blob row aliased alias, or -1 when it holds no BLOB value,
// for scans that cannot afford a query per row. Its text depends on
// whether the store has chunked, so build it per scan.
func StoredLenSQL(q db.Querier, alias string) (string, error) {
	mustAlias(alias)
	inline := "CASE WHEN typeof(" + alias + ".content)='blob' THEN length(" + alias + ".content)"
	ok, err := partsExist(q)
	if err != nil || !ok {
		return inline + " ELSE -1 END", err
	}
	// typeof keeps the column read metadata-only, as the inline arm does.
	return inline + " WHEN typeof(" + alias + ".content)='null' AND " + alias + ".size>=0 THEN " +
		"coalesce((SELECT sum(length(p.content)) FROM blobpart p WHERE p.rid=" + alias + ".rid), -1)" +
		" ELSE -1 END", nil
}

// StoredPrefixSQL returns an SQL expression for the first n bytes of the
// stored value of the blob row aliased alias, NULL when it has none.
func StoredPrefixSQL(q db.Querier, alias string, n int) (string, error) {
	mustAlias(alias)
	if n <= 0 || n > 64 {
		panic("blob.StoredPrefixSQL: n out of range")
	}
	inline := fmt.Sprintf("substr(%s.content, 1, %d)", alias, n)
	ok, err := partsExist(q)
	if err != nil || !ok {
		return inline, err
	}
	return fmt.Sprintf("coalesce(%s, (SELECT substr(p.content, 1, %d) FROM blobpart p"+
		" WHERE p.rid=%s.rid AND p.seq=0))", inline, n, alias), nil
}

func mustAlias(alias string) {
	if alias == "" || len(alias) > 8 {
		panic("blob: alias must be a short identifier")
	}
	for _, c := range alias {
		if c < 'a' || c > 'z' {
			panic("blob: alias must be lowercase letters")
		}
	}
}
