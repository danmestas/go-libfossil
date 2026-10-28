package blob

import (
	"database/sql"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ReadStored returns rid's stored value: the on-disk blob format, of the
// full artifact or of its delta. It returns ErrNotStored for a phantom or a
// row with no content, and ErrPartsCorrupt for damaged parts.
func ReadStored(q db.Querier, rid libfossil.FslID) ([]byte, error) {
	if q == nil || rid <= 0 {
		panic("blob.ReadStored: q must be set and rid > 0")
	}
	return readStored(q, rid)
}

func readStored(q db.Querier, rid libfossil.FslID) ([]byte, error) {
	var size int64
	var content any
	if err := q.QueryRow("SELECT size, content FROM blob WHERE rid=?", rid).
		Scan(&size, &content); err != nil {
		return nil, fmt.Errorf("blob: rid %d: %w", rid, err)
	}
	if size < 0 {
		return nil, fmt.Errorf("blob: rid %d is a phantom: %w", rid, ErrNotStored)
	}
	switch v := content.(type) {
	case []byte:
		if len(v) > 0 {
			return v, nil
		}
	case string: // a TEXT value, which fossil reads as its bytes
		if len(v) > 0 {
			return []byte(v), nil
		}
	case int64:
		return readParts(q, rid, v)
	}
	return nil, fmt.Errorf("blob: rid %d has NULL or empty content: %w", rid, ErrNotStored)
}

// ReadStoredExact returns rid's stored value only if it is length bytes
// long, the length a caller read earlier and budgeted for; a value of any
// other length is ErrStoredChanged and is never loaded.
func ReadStoredExact(q db.Querier, rid libfossil.FslID, length int64) ([]byte, error) {
	if q == nil || rid <= 0 || length <= 0 {
		panic("blob.ReadStoredExact: q must be set, rid and length > 0")
	}
	var size int64
	var inline []byte
	var marker sql.NullInt64
	err := q.QueryRow(`SELECT size,
		CASE WHEN typeof(content)='blob' AND length(content)=?2 THEN content END,
		CASE WHEN typeof(content)='integer' THEN content END
		FROM blob WHERE rid=?1`, rid, length).Scan(&size, &inline, &marker)
	if err != nil {
		return nil, fmt.Errorf("blob: rid %d: %w", rid, err)
	}
	switch {
	case size < 0:
		return nil, fmt.Errorf("blob: rid %d is a phantom: %w", rid, ErrNotStored)
	case len(inline) > 0:
		return inline, nil
	case marker.Valid && marker.Int64 == length:
		return readParts(q, rid, length)
	}
	return nil, fmt.Errorf("blob: rid %d: %w", rid, ErrStoredChanged)
}

// readParts joins rid's parts into the length bytes its row records. Every
// part but the last must be exactly PartBytes and the sequence must run
// unbroken from 0, so a missing, extra or short part is an error.
func readParts(q db.Querier, rid libfossil.FslID, length int64) ([]byte, error) {
	if length <= 0 || length > int64(maxParts)*PartBytes {
		return nil, fmt.Errorf("blob: rid %d records length %d: %w", rid, length, ErrPartsCorrupt)
	}
	n, err := partsFor(length)
	if err != nil {
		panic("blob.readParts: length within maxParts parts")
	}
	rows, err := q.Query("SELECT seq, content FROM blobpart WHERE rid=? ORDER BY seq", rid)
	if err != nil {
		return nil, fmt.Errorf("blob: parts of rid %d: %w", rid, err)
	}
	defer rows.Close()
	out := make([]byte, 0, length)
	seq := 0
	for ; rows.Next(); seq++ {
		if seq >= n {
			return nil, fmt.Errorf("blob: rid %d has over %d parts: %w", rid, n, ErrPartsCorrupt)
		}
		var got int
		var part []byte
		if err := rows.Scan(&got, &part); err != nil {
			return nil, fmt.Errorf("blob: part of rid %d: %w", rid, err)
		}
		want := int(min(PartBytes, length-int64(seq)*PartBytes))
		if got != seq || len(part) != want {
			return nil, fmt.Errorf("blob: rid %d part %d: %w", rid, seq, ErrPartsCorrupt)
		}
		out = append(out, part...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blob: parts of rid %d: %w", rid, err)
	}
	if seq != n || int64(len(out)) != length {
		return nil, fmt.Errorf("blob: rid %d has %d of %d parts: %w", rid, seq, n, ErrPartsCorrupt)
	}
	return out, nil
}

// StoredHead returns the length of rid's stored value and its first n bytes
// (fewer when the value is shorter), without reading the whole value. For a
// chunked row it checks the parts add up to the recorded length.
func StoredHead(q db.Querier, rid libfossil.FslID, n int) (int64, []byte, error) {
	if q == nil || rid <= 0 {
		panic("blob.StoredHead: q must be set and rid > 0")
	}
	if n < 0 || n > PartBytes {
		panic("blob.StoredHead: n out of range")
	}
	var size int64
	var length sql.NullInt64
	var kind string
	var head []byte
	err := q.QueryRow(`SELECT size, typeof(content),
		CASE typeof(content) WHEN 'integer' THEN content ELSE length(content) END,
		CASE WHEN typeof(content) IN ('blob','text') THEN substr(content, 1, ?2) END
		FROM blob WHERE rid=?1`, rid, n).Scan(&size, &kind, &length, &head)
	if err != nil {
		return 0, nil, fmt.Errorf("blob: rid %d: %w", rid, err)
	}
	switch {
	case size < 0:
		return 0, nil, fmt.Errorf("blob: rid %d is a phantom: %w", rid, ErrNotStored)
	case kind == "integer":
		return partsHead(q, rid, length.Int64, n)
	case !length.Valid || length.Int64 == 0:
		return 0, nil, fmt.Errorf("blob: rid %d has NULL or empty content: %w", rid, ErrNotStored)
	}
	return length.Int64, head, nil
}

// partsHead checks rid's parts against the recorded length from their
// lengths alone, which SQLite reads without loading the parts, and returns
// the first n bytes of part 0.
func partsHead(q db.Querier, rid libfossil.FslID, length int64, n int) (int64, []byte, error) {
	if length <= 0 || length > int64(maxParts)*PartBytes {
		return 0, nil, fmt.Errorf("blob: rid %d records length %d: %w", rid, length, ErrPartsCorrupt)
	}
	want, err := partsFor(length)
	if err != nil {
		panic("blob.partsHead: length within maxParts parts")
	}
	var count, lo, hi, total int64
	var head []byte
	err = q.QueryRow(`SELECT count(*), coalesce(min(seq), -1), coalesce(max(seq), -1),
		coalesce(sum(length(content)), 0),
		(SELECT substr(content, 1, ?2) FROM blobpart WHERE rid=?1 AND seq=0)
		FROM blobpart WHERE rid=?1`, rid, n).Scan(&count, &lo, &hi, &total, &head)
	if err != nil {
		return 0, nil, fmt.Errorf("blob: parts of rid %d: %w", rid, err)
	}
	if count != int64(want) || lo != 0 || hi != count-1 || total != length {
		return 0, nil, fmt.Errorf("blob: rid %d has %d parts of %d bytes, want %d of %d: %w",
			rid, count, total, want, length, ErrPartsCorrupt)
	}
	return length, head, nil
}

// StoredLenSQL returns an SQL expression for the length of the stored
// value of the blob row aliased alias, or -1 when it holds none, for scans
// that cannot afford a query per row. A chunked row's length is read from
// its row; ReadStored checks its parts against it.
func StoredLenSQL(alias string) string {
	mustAlias(alias)
	c := alias + ".content"
	return "CASE typeof(" + c + ") WHEN 'blob' THEN length(" + c + ") WHEN 'integer' THEN " + c +
		" ELSE -1 END"
}

// StoredPrefixSQL returns an SQL expression for the first n bytes of the
// stored value of the blob row aliased alias, NULL when it has none. Its
// text depends on whether the store has chunked, so build it per scan.
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
	return fmt.Sprintf("CASE WHEN typeof(%[1]s.content)='integer' THEN (SELECT substr(p.content, 1, %[2]d)"+
		" FROM blobpart p WHERE p.rid=%[1]s.rid AND p.seq=0) ELSE %[3]s END", alias, n, inline), nil
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
