package merge

import "github.com/danmestas/go-libfossil/internal/repo"

func init() { Register(&ConflictFork{}) }

// ConflictFork preserves all divergent versions without merging.
// Used for offline-first scenarios where conflicts accumulate.
// Stores references in a conflict table (Fossil-idiomatic schema).
type ConflictFork struct{}

func (c *ConflictFork) Name() string { return "conflict-fork" }

func (c *ConflictFork) Merge(base, local, remote []byte) (*Result, error) {
	// Don't merge — preserve all versions. The CLI writes to the conflict table.
	return &Result{
		Content: local,
		Clean:   false,
		Conflicts: []Conflict{{
			Local:  local,
			Remote: remote,
			Base:   base,
		}},
	}, nil
}

// EnsureConflictTable creates the conflict table if it doesn't exist.
// Follows Fossil conventions: REFERENCES blob for rids, julianday REAL for timestamps.
func EnsureConflictTable(r *repo.Repo) error {
	if r == nil {
		panic("merge.EnsureConflictTable: r must not be nil")
	}
	if _, err := r.DB().Exec(`CREATE TABLE IF NOT EXISTS conflict(
		cid INTEGER PRIMARY KEY,
		filename TEXT NOT NULL,
		base_rid INTEGER REFERENCES blob,
		local_rid INTEGER REFERENCES blob,
		remote_rid INTEGER REFERENCES blob,
		mtime REAL NOT NULL,
		rid_kind INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return err
	}

	hasRIDKind, err := func() (present bool, err error) {
		rows, err := r.DB().Query("PRAGMA table_info(conflict)")
		if err != nil {
			return false, err
		}
		defer func() {
			if closeErr := rows.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
		}()

		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				return false, err
			}
			if name == "rid_kind" {
				present = true
			}
		}
		if err := rows.Err(); err != nil {
			return false, err
		}
		return present, nil
	}()
	if err != nil {
		return err
	}
	if hasRIDKind {
		return nil
	}

	_, err = r.DB().Exec("ALTER TABLE conflict ADD COLUMN rid_kind INTEGER NOT NULL DEFAULT 0")
	return err
}

// RecordConflictFork inserts a conflict-fork entry.
func RecordConflictFork(r *repo.Repo, filename string, baseRID, localRID, remoteRID int64) error {
	if r == nil {
		panic("merge.RecordConflictFork: r must not be nil")
	}
	if filename == "" {
		panic("merge.RecordConflictFork: filename must not be empty")
	}
	_, err := r.DB().Exec(
		"INSERT INTO conflict(filename, base_rid, local_rid, remote_rid, mtime, rid_kind) VALUES(?, ?, ?, ?, julianday('now'), 1)",
		filename, baseRID, localRID, remoteRID,
	)
	return err
}

// ResolveConflictFork deletes a resolved entry (Fossil convention: delete, don't flag).
func ResolveConflictFork(r *repo.Repo, filename string) error {
	if r == nil {
		panic("merge.ResolveConflictFork: r must not be nil")
	}
	if filename == "" {
		panic("merge.ResolveConflictFork: filename must not be empty")
	}
	_, err := r.DB().Exec("DELETE FROM conflict WHERE filename=?", filename)
	return err
}

// ListConflictForks returns all unresolved conflict-fork entries.
func ListConflictForks(r *repo.Repo) ([]string, error) {
	if r == nil {
		panic("merge.ListConflictForks: r must not be nil")
	}
	// Check if conflict table exists.
	var count int
	err := r.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='conflict'").Scan(&count)
	if err != nil || count == 0 {
		return nil, nil
	}

	rows, err := r.DB().Query("SELECT filename FROM conflict ORDER BY mtime DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
