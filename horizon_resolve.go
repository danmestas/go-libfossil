package libfossil

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// symTag resolves a symbolic name: the newest check-in carrying sym-NAME,
// else, in a store with a structural horizon, the check-in below the
// horizon the tag names (draft-fossil-horizon-00 §5.5). The store holds the
// tag's control artifact but not the check-in, so it resolves to that
// check-in's hash, and an operation on it reports the horizon and the
// deepen rather than an unknown name.
func (rv resolver) symTag(name string) (int64, error) {
	if rid, err := newestWithSymTag(rv.h, name, rv.ciOnly); err != nil || rid != 0 {
		return rid, err
	}
	return symTagBelowHorizon(rv.h, name)
}

// symTagBelowHorizon is the check-in below the store's structural horizon
// that carries sym-NAME, by the newest tag event; 0 when there is none.
func symTagBelowHorizon(h *db.DB, name string) (int64, error) {
	if _, structural, err := content.StructuralHorizon(h); err != nil || !structural {
		return 0, err
	}
	var rid int64
	err := h.QueryRow(`SELECT tagxref.rid FROM tag
		JOIN tagxref ON tagxref.tagid = tag.tagid
		JOIN blob ON blob.rid = tagxref.rid
		WHERE tag.tagname = ? AND tagxref.tagtype > 0 AND blob.size < 0
		ORDER BY tagxref.mtime DESC LIMIT 1`, "sym-"+name).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	_, below, err := content.BelowHorizon(context.Background(), h, fsltype.FslID(rid))
	if err != nil || !below {
		return 0, err
	}
	return rid, nil
}
