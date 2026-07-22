package uv

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// Entry represents a row in the unversioned table.
type Entry struct {
	Name  string
	MTime int64  // seconds since 1970
	Hash  string // "" for tombstone (NULL in DB)
	Size  int    // uncompressed size
}

const schemaUV = `CREATE TABLE IF NOT EXISTS unversioned(
  uvid INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT UNIQUE,
  rcvid INTEGER,
  mtime DATETIME,
  hash TEXT,
  sz INTEGER,
  encoding INT,
  content BLOB
);`

func EnsureSchema(d *db.DB) error {
	if d == nil {
		panic("uv.EnsureSchema: d must not be nil")
	}
	_, err := d.Exec(schemaUV)
	return err
}

func Write(d *db.DB, name string, content []byte, mtime int64) error {
	if d == nil {
		panic("uv.Write: d must not be nil")
	}
	if name == "" {
		panic("uv.Write: name must not be empty")
	}
	if content == nil {
		panic("uv.Write: content must not be nil")
	}

	// Detect repo hash policy. Default to SHA1; use SHA3 if project-code is 64-char.
	var projCode string
	_ = d.QueryRow("SELECT value FROM config WHERE name='project-code'").Scan(&projCode)
	referenceHash := "0000000000000000000000000000000000000000" // 40-char = SHA1
	if len(projCode) > 40 {
		referenceHash = projCode
	}
	contentHash := hash.ContentHash(content, referenceHash)
	sz := len(content)

	// Compress and check 80% threshold.
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	w.Write(content)
	w.Close()

	var encoding int
	var stored []byte
	if compressed.Len() <= sz*4/5 { // <= 80%
		encoding = 1
		stored = compressed.Bytes()
	} else {
		encoding = 0
		stored = content
	}

	_, err := d.Exec(
		`REPLACE INTO unversioned(name, rcvid, mtime, hash, sz, encoding, content)
		 VALUES(?, 1, ?, ?, ?, ?, ?)`,
		name, mtime, contentHash, sz, encoding, stored,
	)
	if err != nil {
		return fmt.Errorf("uv.Write: %w", err)
	}
	return InvalidateHash(d)
}

func Delete(d *db.DB, name string, mtime int64) error {
	if d == nil {
		panic("uv.Delete: d must not be nil")
	}
	if name == "" {
		panic("uv.Delete: name must not be empty")
	}

	// Check if row exists; if not, insert tombstone.
	var exists int
	if err := d.QueryRow("SELECT count(*) FROM unversioned WHERE name=?", name).Scan(&exists); err != nil {
		return fmt.Errorf("uv.Delete: check existence: %w", err)
	}
	if exists == 0 {
		_, err := d.Exec(
			`INSERT INTO unversioned(name, rcvid, mtime, hash, sz, encoding, content)
			 VALUES(?, 1, ?, NULL, 0, 0, NULL)`, name, mtime,
		)
		if err != nil {
			return fmt.Errorf("uv.Delete insert: %w", err)
		}
	} else {
		_, err := d.Exec(
			`UPDATE unversioned SET rcvid=1, mtime=?, hash=NULL, sz=0, encoding=0, content=NULL
			 WHERE name=?`, mtime, name,
		)
		if err != nil {
			return fmt.Errorf("uv.Delete update: %w", err)
		}
	}
	return InvalidateHash(d)
}

func Read(d *db.DB, name string) ([]byte, int64, string, error) {
	if d == nil {
		panic("uv.Read: d must not be nil")
	}

	var mtime int64
	var hashVal sql.NullString
	var encoding int
	var stored []byte

	err := d.QueryRow(
		"SELECT CAST(mtime AS INTEGER), hash, encoding, content FROM unversioned WHERE name=?", name,
	).Scan(&mtime, &hashVal, &encoding, &stored)
	if err == sql.ErrNoRows {
		return nil, 0, "", nil
	}
	if err != nil {
		return nil, 0, "", fmt.Errorf("uv.Read: %w", err)
	}

	h := ""
	if hashVal.Valid {
		h = hashVal.String
	}

	// Tombstone: hash is NULL
	if !hashVal.Valid {
		return nil, mtime, "", nil
	}

	// Decompress if needed.
	if encoding == 1 && stored != nil {
		r, err := zlib.NewReader(bytes.NewReader(stored))
		if err != nil {
			return nil, 0, "", fmt.Errorf("uv.Read: zlib open: %w", err)
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, 0, "", fmt.Errorf("uv.Read: zlib read: %w", err)
		}
		return data, mtime, h, nil
	}

	return stored, mtime, h, nil
}

func List(d *db.DB) ([]Entry, error) {
	if d == nil {
		panic("uv.List: d must not be nil")
	}

	rows, err := d.Query("SELECT name, CAST(mtime AS INTEGER), hash, sz FROM unversioned ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("uv.List: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var hashVal sql.NullString
		if err := rows.Scan(&e.Name, &e.MTime, &hashVal, &e.Size); err != nil {
			return nil, err
		}
		if hashVal.Valid {
			e.Hash = hashVal.String
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ContentHash computes the SHA1 catalog hash matching Fossil's
// unversioned_content_hash(). Always SHA1, even in SHA3 repos.
// Format: "name YYYY-MM-DD HH:MM:SS hash\n" per file, sorted by name.
func ContentHash(d *db.DB) (string, error) {
	if d == nil {
		panic("uv.ContentHash: d must not be nil")
	}

	// Check cache first.
	var cached string
	err := d.QueryRow("SELECT value FROM config WHERE name='uv-hash'").Scan(&cached)
	if err == nil && cached != "" {
		return cached, nil
	}

	// Compute.
	rows, err := d.Query(
		`SELECT name, datetime(mtime,'unixepoch'), hash
		 FROM unversioned WHERE hash IS NOT NULL ORDER BY name`,
	)
	if err != nil {
		return "", fmt.Errorf("uv.ContentHash: query: %w", err)
	}
	defer rows.Close()

	h := sha1.New()
	for rows.Next() {
		var name, dt, fileHash string
		if err := rows.Scan(&name, &dt, &fileHash); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %s %s\n", name, dt, fileHash)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	result := hex.EncodeToString(h.Sum(nil))

	// Cache. Non-fatal: if this fails we just recompute next time.
	if _, err := d.Exec(
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES('uv-hash', ?, strftime('%s','now'))",
		result,
	); err != nil {
		return result, fmt.Errorf("uv.ContentHash: cache write: %w", err)
	}

	return result, nil
}

func InvalidateHash(d *db.DB) error {
	if d == nil {
		panic("uv.InvalidateHash: d must not be nil")
	}
	_, err := d.Exec("DELETE FROM config WHERE name='uv-hash'")
	return err
}
