package content

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// RequireAvailableByUUID is a local-only read gate. Missing content is not a
// missing path or corruption: sparse stores deliberately retain some absences.
// Do not claim that no peer exists; a read has no authority to use a saved peer.
func RequireAvailableByUUID(q db.Querier, uuid string) (libfossil.FslID, error) {
	rid, held, err := CheckAvailableByUUID(q, uuid)
	if err != nil {
		return 0, err
	}
	if held {
		return rid, nil
	}
	var shunned bool
	if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM shun WHERE uuid=?)", uuid).
		Scan(&shunned); err != nil {
		return 0, err
	}
	if shunned {
		return 0, fmt.Errorf("required content %s is shunned", uuid)
	}
	absences, err := LoadAbsences(q)
	if err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("required content %s is unavailable; "+
		"%d elided artifact(s), %d transit artifact(s); "+
		"local-only read: widen/deepen and sync from a peer", uuid,
		absences.Count(Elided), absences.Count(Transit))
}
