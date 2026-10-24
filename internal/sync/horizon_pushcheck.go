package sync

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
)

// staleCancel is the push-time check of draft-fossil-horizon-00 §5.7, run
// over what the request stored once it is crosslinked. A branch-creating
// check-in cancels its parent's branch by the name its clone derived, which
// under a structural horizon came from the seed. When this origin derives
// the parent on another branch, the seed was stale: staleCancel returns the
// refusal, which rolls the whole push back. A check-in is admitted when this
// push or the store holds a control artifact cancelling the parent's real
// branch on it (the correction), and so is one whose parent this origin
// cannot place on a branch: the check protects against a wrong seed, as
// zit's pushSeedCancelFault does, not against every unusual push.
func (h *handler) staleCancel() (string, error) {
	q := h.repo.DB()
	for _, uuid := range h.received {
		rid, ok := blob.Exists(q, uuid)
		if !ok {
			continue
		}
		var checkin bool
		if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM event WHERE objid=? AND type='ci')", rid).Scan(&checkin); err != nil {
			return "", fmt.Errorf("handler: push check: %w", err)
		}
		if !checkin {
			continue
		}
		data, err := content.Expand(q, rid)
		if err != nil {
			return "", fmt.Errorf("handler: push check: %w", err)
		}
		d, err := deck.Parse(data)
		if err != nil || len(d.P) == 0 {
			continue
		}
		for _, t := range d.T {
			cancelled, ok := strings.CutPrefix(t.Name, "sym-")
			if t.Type != deck.TagCancel || t.UUID != "*" || !ok {
				continue
			}
			on, placed, err := branchOfCheckin(q, d.P[0])
			if err != nil {
				return "", err
			}
			if !placed || on == cancelled {
				continue
			}
			corrected, err := cancelledOn(q, int64(rid), "sym-"+on)
			if err != nil {
				return "", err
			}
			if corrected {
				continue
			}
			return fmt.Sprintf("stale seed: the parent of %s is on branch '%s', not '%s'; re-sync, then push "+
				"a control artifact cancelling sym-%s on %s (a move to its branch) with it",
				abbrev(uuid), on, cancelled, on, uuid), nil
		}
	}
	return "", nil
}

// seedRefusedText is the error an origin refuses a pushed seed with (horizon
// §5.2.3, as the zit 0.7.1 erratum words it).
const seedRefusedText = "seed refused: a seed is private to the clone that holds it; it is never pushed"

// ownSeedReadable reports whether a delta this request stored before its
// basis was readable has become readable and is a seed (horizon §5.2.3):
// the seed is judged within the request that carried it, which is then
// refused whole. It reads the bytes, since the cascade that judged the
// delta when its basis arrived refused the seed's hash but cannot refuse
// the request; that cascade also cleared the pending row of a delta that is
// not a seed. A delta still unreadable stays pending.
func (h *handler) ownSeedReadable() (bool, error) {
	if len(h.danglingHere) == 0 {
		return false, nil
	}
	if h.pass == nil {
		panic("handler.ownSeedReadable: a stored delta implies a receive pass")
	}
	q := h.repo.DB()
	for _, d := range h.danglingHere {
		if !content.IsAvailable(q, d.rid) {
			continue
		}
		// The pass's basis cache keeps every node it rebuilds, so a chain of
		// deltas is expanded once along its length, not once per delta.
		data, err := h.pass.basis.ExpandUnchecked(q, d.rid)
		if err != nil {
			return false, fmt.Errorf("handler: seed check: %w", err)
		}
		if seedcheck.IsSeedBytes(data) {
			return true, nil
		}
	}
	return false, nil
}

// branchOfCheckin is the branch this repository derives for the check-in
// named uuid; placed is false when it holds no branch row for it.
func branchOfCheckin(q db.Querier, uuid string) (string, bool, error) {
	var on string
	err := q.QueryRow(`SELECT tagxref.value FROM tagxref JOIN tag USING(tagid) JOIN blob ON blob.rid=tagxref.rid
		WHERE blob.uuid=? AND tag.tagname='branch' AND tagxref.tagtype>0`, uuid).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("handler: push check: %w", err)
	}
	return on, true, nil
}

// cancelledOn reports a control artifact's cancellation of tag on rid: a
// cancel row sourced from an artifact other than rid itself.
func cancelledOn(q db.Querier, rid int64, tag string) (bool, error) {
	var found bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM tagxref JOIN tag USING(tagid)
		WHERE tagxref.rid=? AND tag.tagname=? AND tagxref.tagtype=0
		AND tagxref.srcid>0 AND tagxref.srcid<>tagxref.rid)`, rid, tag).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("handler: push check: %w", err)
	}
	return found, nil
}

// abbrev is a hash's first ten digits, as a diagnostic names a check-in.
func abbrev(uuid string) string {
	return uuid[:min(len(uuid), 10)]
}
