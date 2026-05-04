package libfossil

import (
	"database/sql"
	"errors"
	"fmt"
)

// UUIDFromRID returns the UUID (manifest hash) of the artifact identified by
// rid. The UUID is the stable, content-addressed identifier for the artifact;
// the rid is a repository-local integer that may differ across clones.
//
// Returns ErrArtifactNotFound (wrapped) if no artifact with the given rid
// exists in the repository. The wrapped error message includes the offending
// rid. Other errors surface as-is.
func (r *Repo) UUIDFromRID(rid int64) (string, error) {
	uuid, err := r.inner.UUIDByRID(rid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("libfossil: uuid from rid %d: %w", rid, ErrArtifactNotFound)
		}
		return "", fmt.Errorf("libfossil: uuid from rid %d: %w", rid, err)
	}
	return uuid, nil
}
