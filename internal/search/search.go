package search

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/repo"
)

// Index manages the FTS5 trigram index in a repo DB.
type Index struct {
	repo *repo.Repo
}

// Open creates an Index from an open repo. Creates FTS5 tables if they don't exist.
//
// Panics if r is nil (TigerStyle precondition).
func Open(r *repo.Repo) (*Index, error) {
	if r == nil {
		panic("search.Open: nil *repo.Repo")
	}

	if err := ensureSchema(r); err != nil {
		return nil, fmt.Errorf("search.Open: %w", err)
	}

	return &Index{repo: r}, nil
}

// Drop removes the FTS tables entirely.
func (idx *Index) Drop() error {
	if idx == nil {
		panic("search.Drop: nil *Index")
	}
	db := idx.repo.DB()
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS fts_content",
		"DROP TABLE IF EXISTS fts_meta",
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("search.Drop: %w", err)
		}
	}
	return nil
}

// ensureSchema creates FTS5 and metadata tables if they don't already exist.
// Idempotent — safe to call on every Open because CREATE IF NOT EXISTS is a no-op
// when tables are present, avoiding the need for a separate migration path.
func ensureSchema(r *repo.Repo) error {
	db := r.DB()
	stmts := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS fts_content USING fts5(
			path,
			content,
			tokenize='trigram'
		)`,
		`CREATE TABLE IF NOT EXISTS fts_meta(
			key TEXT PRIMARY KEY,
			value TEXT
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("ensureSchema: %w", err)
		}
	}
	return nil
}

// Query configures a search request.
type Query struct {
	Term         string // search term (min 3 chars, FTS5 special chars escaped internally)
	MaxResults   int    // 0 → default (50)
	ContextLines int    // lines of surrounding context (0 → just the match line)
}

// Result is a single search hit.
type Result struct {
	Path     string // file pathname
	Line     int    // 1-based line number
	Column   int    // 0-based byte offset within the line
	MatchLen int    // length of matched substring
	LineText string // the matching line
	Context  string // surrounding lines including match line, newline-separated. Empty if ContextLines=0.
}
