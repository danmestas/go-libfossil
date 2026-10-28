package blob

import (
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// A row's stored value is the on-disk blob format: Compress's output, or
// wire bytes already in that format. It normally lives in blob.content.
// When the backend caps value size (db.DriverConfig.MaxValueBytes) and the
// value exceeds the cap, blob.content holds the value's length as an
// INTEGER and the value is split, in order, across blobpart rows keyed by
// (rid, seq). size stays the artifact's expanded length, so a chunked row
// is never mistaken for a phantom (size < 0), and the INTEGER tells a
// chunked row from a torn one whose content is NULL. Reads check the parts
// against that length, so damaged parts are an error, never short data.
// Chunking is purely physical: it sits below compression, so hashes,
// deltas and the wire format are unchanged.
//
// Everything that reads or writes blob.content goes through this file.

// PartBytes is the size of every part of a chunked value but the last.
const PartBytes = 1_000_000

// maxParts bounds a chunked value at about 4 GB, past what the 4-byte size
// prefix of the blob format can describe.
const maxParts = 4096

// ErrNotStored reports a row with no stored value: a phantom, or a row
// whose content is NULL.
var ErrNotStored = errors.New("blob: no stored content")

// ErrPartsCorrupt reports a chunked row whose parts do not add up to the
// length its row records.
var ErrPartsCorrupt = errors.New("blob: chunked value is corrupt")

// ErrStoredChanged reports a stored value that is not the length a caller
// read earlier and budgeted for.
var ErrStoredChanged = errors.New("blob: stored value changed")

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

// partsFor is the number of parts a value of length bytes takes. Stored
// values can come from peers, so one too large is an error, not a panic.
func partsFor(length int64) (int, error) {
	if length <= 0 {
		panic("blob.partsFor: length must be > 0")
	}
	n := (length + PartBytes - 1) / PartBytes
	if n > maxParts {
		return 0, fmt.Errorf("blob: a %d-byte stored value needs over %d parts", length, maxParts)
	}
	return int(n), nil
}

// atomically runs fn so that its writes land together or not at all: in a
// savepoint of a transaction, or in a transaction of its own on a DB. A
// chunked write is several statements, and a torn one would look held.
func atomically(q db.Querier, fn func(q db.Querier) error) error {
	switch t := q.(type) {
	case *db.Tx:
		return t.WithSavepoint(func(tx *db.Tx) error { return fn(tx) })
	case *db.DB:
		return t.WithTx(func(tx *db.Tx) error { return fn(tx) })
	}
	return fn(q)
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

// writeParts replaces rid's parts with stored, split into n PartBytes pieces.
func writeParts(q db.Querier, rid libfossil.FslID, stored []byte, n int) error {
	if n < 2 || n > maxParts || (n-1)*PartBytes >= len(stored) || n*PartBytes < len(stored) {
		panic("blob.writeParts: part count does not fit stored")
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

// isChunked reports whether rid's row is chunked, so has parts to drop.
func isChunked(q db.Querier, rid libfossil.FslID) (bool, error) {
	var kind string
	err := q.QueryRow("SELECT typeof(content) FROM blob WHERE rid=?", rid).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("blob: rid %d: %w", rid, err)
	}
	return kind == "integer", nil
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
	if chunked(stored) {
		n, err := partsFor(int64(len(stored)))
		if err != nil {
			return err
		}
		return atomically(q, func(q db.Querier) error {
			if err := updateContent(q, rid, int64(len(stored))); err != nil {
				return err
			}
			return writeParts(q, rid, stored, n)
		})
	}
	had, err := isChunked(q, rid)
	if err != nil {
		return err
	}
	if err := updateContent(q, rid, stored); err != nil {
		return err
	}
	if !had {
		return nil
	}
	// Parts left behind by a failure here are harmless: a BLOB content is
	// read inline, and SweepParts removes them.
	if _, err := q.Exec("DELETE FROM blobpart WHERE rid=?", rid); err != nil {
		return fmt.Errorf("blob.WriteStored: drop parts rid=%d: %w", rid, err)
	}
	return nil
}

// updateContent sets rid's content to value: the stored bytes, or the
// length of a chunked value.
func updateContent(q db.Querier, rid libfossil.FslID, value any) error {
	res, err := q.Exec("UPDATE blob SET content=? WHERE rid=?", value, rid)
	if err != nil {
		return fmt.Errorf("blob.WriteStored rid=%d: %w", rid, err)
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("blob.WriteStored: no row rid=%d", rid)
	}
	return nil
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
	if !chunked(stored) {
		return insertRow(q, uuid, size, stored, rcvid)
	}
	n, err := partsFor(int64(len(stored)))
	if err != nil {
		return 0, err
	}
	var rid libfossil.FslID
	err = atomically(q, func(q db.Querier) error {
		var err error
		if rid, err = insertRow(q, uuid, size, int64(len(stored)), rcvid); err != nil {
			return err
		}
		// A rid can be reused after its row was deleted; writeParts clears
		// any parts left under it first.
		return writeParts(q, rid, stored, n)
	})
	if err != nil {
		return 0, err
	}
	return rid, nil
}

func insertRow(q db.Querier, uuid string, size int64, content any, rcvid int64) (libfossil.FslID, error) {
	res, err := q.Exec("INSERT INTO blob(uuid, size, content, rcvid) VALUES(?, ?, ?, ?)",
		uuid, size, content, rcvid)
	if err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("lastid: %w", err)
	}
	return libfossil.FslID(id), nil
}

// DropStored turns rid into a phantom: size -1, no content, no parts. The
// caller records it in the phantom table if it wants it requested.
func DropStored(q db.Querier, rid libfossil.FslID) error {
	if q == nil || rid <= 0 {
		panic("blob.DropStored: q must be set and rid > 0")
	}
	had, err := isChunked(q, rid)
	if err != nil {
		return err
	}
	if _, err := q.Exec("UPDATE blob SET size=-1, content=NULL WHERE rid=?", rid); err != nil {
		return fmt.Errorf("blob.DropStored rid=%d: %w", rid, err)
	}
	if !had {
		return nil
	}
	if _, err := q.Exec("DELETE FROM blobpart WHERE rid=?", rid); err != nil {
		return fmt.Errorf("blob.DropStored: parts rid=%d: %w", rid, err)
	}
	return nil
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
		(SELECT rid FROM blob WHERE typeof(content)='integer')`)
	if err != nil {
		return fmt.Errorf("blob.SweepParts: %w", err)
	}
	return nil
}
