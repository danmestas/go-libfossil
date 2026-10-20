package repo

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
)

// DefaultMainBranch is the default branch of a repository whose main-branch
// setting is unset.
const DefaultMainBranch = "trunk"

// MainBranch is the repository's default branch: the main-branch setting, or
// DefaultMainBranch when that is unset. draft-fossil-repo-state-00 Section 6.1
// attributes a check-in with no active branch tag to this branch and forbids
// assuming "trunk" when the setting is present. An empty value is read as
// unset (no branch has an empty name), which departs from the draft and from
// fossil's db_get, which would return the empty string.
func MainBranch(q db.Querier) (string, error) {
	if q == nil {
		panic("repo.MainBranch: q must not be nil")
	}
	var value sql.NullString
	err := q.QueryRow("SELECT value FROM config WHERE name = 'main-branch'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && value.String == "") {
		return DefaultMainBranch, nil
	}
	if err != nil {
		return "", fmt.Errorf("repo.MainBranch: %w", err)
	}
	return value.String, nil
}
