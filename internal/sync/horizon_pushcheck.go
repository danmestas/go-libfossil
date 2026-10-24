package sync

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/shun"
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

// seedTag is the tag name of a seed's binding card; no pushed control
// artifact may carry it (horizon §5.2.3).
const seedTag = "horizon-seed"

// isSeed reports whether data, an artifact's full content, is a seed this
// origin must refuse (horizon §5.2.3): a control artifact whose U card is
// its own seed label, or one carrying a horizon-seed T card under any label.
// Another implementation's label alone is ordinary, and so is a user's login
// unless it equals seedLogin.
func isSeed(data []byte) bool {
	if data == nil {
		panic("sync.isSeed: data must not be nil")
	}
	labelled := bytes.Contains(data, []byte("\nU "+seedLogin+"\n"))
	if !labelled && !bytes.Contains(data, []byte(seedTag)) {
		return false
	}
	d, err := deck.Parse(data)
	if err != nil || d.Type != deck.Control {
		return false
	}
	if d.U != nil && *d.U == seedLogin {
		return true
	}
	for _, t := range d.T {
		if t.Name == seedTag {
			return true
		}
	}
	return false
}

// seedShunComment is the shun table's comment on a seed recorded as refused.
const seedShunComment = "horizon seed refused (§5.2.3)"

// judgeGrounded judges, as seeds (horizon §5.2.3), the dangling deltas that
// filling the phantom filled has made readable. The test is made on the
// artifact when it first becomes readable, whatever request carried it: a
// seed this request carried refuses the request whole; one an earlier
// request left dangling is recorded as shunned, so it is never crosslinked,
// announced or sent, while this request, which only supplied its basis, is
// stored as usual. A delta whose bytes do not hash to its name is not the
// artifact it names and is left as it is: shunning that name would let a
// forged delta bar the real artifact.
func (h *handler) judgeGrounded(filled libfossil.FslID) error {
	if filled <= 0 {
		panic("handler.judgeGrounded: filled must be positive")
	}
	q := h.repo.DB()
	if !content.IsAvailable(q, filled) {
		return nil // still dangling itself: nothing it grounds is readable
	}
	deps, err := dependentDeltas(q, []libfossil.FslID{filled})
	if err != nil {
		return err
	}
	for _, rid := range deps {
		data, err := content.Expand(q, rid)
		if errors.Is(err, content.ErrHashMismatch) {
			continue
		}
		if err != nil {
			return fmt.Errorf("handler: seed check: %w", err)
		}
		if !isSeed(data) {
			continue
		}
		if h.danglingHere[rid] {
			h.fail(seedRefusedText)
			return nil
		}
		var uuid string
		if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			return fmt.Errorf("handler: seed check: %w", err)
		}
		if err := shun.Add(q, uuid, seedShunComment); err != nil {
			return fmt.Errorf("handler: seed check: %w", err)
		}
	}
	return nil
}

// dependentDeltas is every row stored as a delta on one of roots, directly
// or through other deltas: what filling roots can make readable.
func dependentDeltas(q db.Querier, roots []libfossil.FslID) ([]libfossil.FslID, error) {
	seen := make(map[libfossil.FslID]bool, len(roots))
	queue := slices.Clone(roots)
	var out []libfossil.FslID
	for steps := 0; len(queue) > 0; steps++ {
		if steps > maxChainSteps {
			return nil, fmt.Errorf("handler: seed check: delta dependents do not end")
		}
		cur := queue[0]
		queue = queue[1:]
		rows, err := q.Query("SELECT rid FROM delta WHERE srcid=?", cur)
		if err != nil {
			return nil, fmt.Errorf("handler: seed check: %w", err)
		}
		for rows.Next() {
			var rid libfossil.FslID
			if err := rows.Scan(&rid); err != nil {
				rows.Close()
				return nil, fmt.Errorf("handler: seed check: %w", err)
			}
			if !seen[rid] {
				seen[rid] = true
				out = append(out, rid)
				queue = append(queue, rid)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("handler: seed check: %w", err)
		}
	}
	return out, nil
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
