package checkout

import (
	"database/sql"
	"fmt"
)

const vfileSchema = `
CREATE TABLE IF NOT EXISTS vfile(
  id       INTEGER PRIMARY KEY,
  vid      INTEGER NOT NULL,
  chnged   INTEGER DEFAULT 0,
  deleted  INTEGER DEFAULT 0,
  isexe    INTEGER DEFAULT 0,
  islink   INTEGER DEFAULT 0,
  rid      INTEGER DEFAULT 0,
  mrid     INTEGER DEFAULT 0,
  mtime    INTEGER DEFAULT 0,
  pathname TEXT NOT NULL,
  origname TEXT,
  mhash    TEXT,
  UNIQUE(pathname, vid)
);
`

const vmergeSchema = `
CREATE TABLE IF NOT EXISTS vmerge(
  id    INTEGER REFERENCES vfile,
  merge INTEGER,
  mhash TEXT
);
`

const vvarSchema = `
CREATE TABLE IF NOT EXISTS vvar(
  name  TEXT PRIMARY KEY,
  value TEXT
);
`

// EnsureTables creates the vfile, vmerge, and vvar tables if they don't exist.
// This is idempotent — safe to call multiple times.
//
// Panics if db is nil (TigerStyle precondition).
func EnsureTables(db *sql.DB) error {
	if db == nil {
		panic("checkout.EnsureTables: nil *sql.DB")
	}

	schemas := []string{vfileSchema, vmergeSchema, vvarSchema}
	for _, schema := range schemas {
		if _, err := db.Exec(schema); err != nil {
			return fmt.Errorf("checkout.EnsureTables: %w", err)
		}
	}
	return nil
}

// getVVar retrieves a vvar value by name. Returns empty string if not found.
func getVVar(db *sql.DB, name string) (string, error) {
	if db == nil {
		panic("checkout.getVVar: nil *sql.DB")
	}

	var value string
	err := db.QueryRow("SELECT value FROM vvar WHERE name = ?", name).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("checkout.getVVar: %w", err)
	}
	return value, nil
}

// sqlExecer is what the checkout writes its tables through: the checkout
// database, or a transaction on it.
type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// setVVar sets a vvar value, replacing any existing value.
func setVVar(db sqlExecer, name, value string) error {
	if db == nil {
		panic("checkout.setVVar: nil database")
	}

	_, err := db.Exec("INSERT OR REPLACE INTO vvar (name, value) VALUES (?, ?)", name, value)
	if err != nil {
		return fmt.Errorf("checkout.setVVar: %w", err)
	}
	return nil
}
