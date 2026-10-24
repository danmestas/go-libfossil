package horizon

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// ErrNameNotFound is returned when a horizon name resolves to no check-in.
var ErrNameNotFound = errors.New("horizon: name resolves to no check-in")

// NameTip is the name of the most recent check-in.
const NameTip = "tip"

// Resolved is the check-in a horizon name resolved to and how many steps
// back along primary parents were taken to reach it.
type Resolved struct {
	Hash  string
	Steps uint32
}

// ResolveName resolves a horizon name over q's check-ins as
// draft-fossil-horizon-00 §6.2.7 does: tip is the most recent check-in, a
// full hash names its check-in, and any other name, or a full hash naming no
// check-in, is the most recent check-in carrying an active sym-NAME tag,
// propagated or not. A tie on mtime goes to the lexicographically lowest
// full hash, never a local rid, so every origin resolves a name alike. From there it steps back up
// to depth times along primary parents, stopping early at a check-in with
// none.
func ResolveName(q db.Querier, name string, depth uint32) (Resolved, error) {
	if q == nil {
		panic("horizon.ResolveName: nil querier")
	}
	rid, err := namedCheckin(q, name)
	if err != nil {
		return Resolved{}, err
	}
	var steps uint32
	for steps < depth {
		var parent libfossil.FslID
		err := q.QueryRow(`SELECT p.pid FROM plink p JOIN event e ON e.objid=p.pid AND e.type='ci'
			WHERE p.cid=? AND p.isprim=1`, rid).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return Resolved{}, fmt.Errorf("horizon: primary parent: %w", err)
		}
		if parent == rid {
			panic("horizon.ResolveName: a check-in is its own primary parent")
		}
		rid, steps = parent, steps+1
	}
	var uuid string
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		return Resolved{}, fmt.Errorf("horizon: resolved check-in: %w", err)
	}
	return Resolved{Hash: uuid, Steps: steps}, nil
}

// namedCheckin is the check-in name names before any step back.
func namedCheckin(q db.Querier, name string) (libfossil.FslID, error) {
	if name == NameTip {
		return firstCheckin(q, `SELECT e.objid FROM event e JOIN blob b ON b.rid=e.objid
			WHERE e.type='ci' ORDER BY e.mtime DESC, b.uuid ASC LIMIT 1`)
	}
	if isFullHash(name) {
		rid, err := firstCheckin(q, `SELECT e.objid FROM blob b JOIN event e ON e.objid=b.rid AND e.type='ci'
			WHERE b.uuid=?`, strings.ToLower(name))
		if !errors.Is(err, ErrNameNotFound) {
			return rid, err
		}
	}
	return firstCheckin(q, `SELECT e.objid FROM tagxref x JOIN tag t ON t.tagid=x.tagid
		JOIN event e ON e.objid=x.rid AND e.type='ci' JOIN blob b ON b.rid=e.objid
		WHERE t.tagname=? AND x.tagtype>0 ORDER BY e.mtime DESC, b.uuid ASC LIMIT 1`, "sym-"+name)
}

// firstCheckin is the rid query's first row names, ErrNameNotFound for none.
func firstCheckin(q db.Querier, query string, args ...any) (libfossil.FslID, error) {
	var rid libfossil.FslID
	err := q.QueryRow(query, args...).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNameNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("horizon: resolve name: %w", err)
	}
	return rid, nil
}

// isFullHash reports a 40 or 64 digit hex string, of either case.
func isFullHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
