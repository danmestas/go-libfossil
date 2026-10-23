package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// horizonMergeBasePragma asks an origin for Fossil's pivot of a primary
// check-in over one or two secondaries, `pragma horizon-mergebase PRIMARY
// SECONDARY...`, and carries its answer, the query's hashes repeated in the
// order asked with the base appended, - when they share no ancestor
// (draft-fossil-horizon-00 §6.2.4).
const horizonMergeBasePragma = "horizon-mergebase"

// noMergeBase is the answer naming no common ancestor.
const noMergeBase = "-"

// maxMergeBaseSecondaries is the most secondaries one query carries. A card
// handler sees six tokens, so an answer repeats at most three hashes before
// the base (§6.2.4); an origin answers no query naming more.
const maxMergeBaseSecondaries = 2

// mergeBaseNodes bounds the check-ins one merge-base answer visits (§8: the
// bound of the origin's other graph walks, horizon.HeldSet's among them).
const mergeBaseNodes = 1 << 20

// MergeBaseQuery is one merge-base question (§6.2.4), every check-in named
// by full hash: in a merge the primary is the check-in merged in, and the
// secondaries the checkout's version and each plain merge pending in it.
// The zero value asks nothing.
type MergeBaseQuery struct {
	Primary     string
	Secondaries []string
}

// hashes is the query as the wire spells it, primary first.
func (q MergeBaseQuery) hashes() []string {
	return append([]string{q.Primary}, q.Secondaries...)
}

// valid reports whether q is a query a card can carry: a primary and one
// or two secondaries, every one a well-formed hash.
func (q MergeBaseQuery) valid() bool {
	if len(q.Secondaries) == 0 || len(q.Secondaries) > maxMergeBaseSecondaries {
		return false
	}
	for _, h := range q.hashes() {
		if !hash.IsValidHash(h) {
			return false
		}
	}
	return true
}

// parseMergeBaseQuery reads a merge-base query, or ok false, which leaves
// the pragma ignored like any it does not know: a malformed hash, or more
// secondaries than an answer could repeat.
func parseMergeBaseQuery(p *xfer.PragmaCard) (MergeBaseQuery, bool) {
	if len(p.Values) < 2 {
		return MergeBaseQuery{}, false
	}
	q := MergeBaseQuery{Primary: p.Values[0], Secondaries: slices.Clone(p.Values[1:])}
	return q, q.valid()
}

// answerMergeBase appends the answer to the request's merge-base query, if
// it carried one this origin can answer. It answers in a pull's reply only,
// where zit answers it, so the requester has read access to the history the
// base is drawn from.
func (h *handler) answerMergeBase(ctx context.Context) error {
	q := h.mergeBaseQuery
	if q.Primary == "" || !h.pullOK {
		return nil
	}
	base, ok, err := originMergeBase(ctx, h.repo, q, mergeBaseNodes)
	if errors.Is(err, merge.ErrBound) {
		return nil // unanswered: the client reads it as "deepen by hand"
	}
	if err != nil || !ok {
		return err
	}
	h.resp = append(h.resp, &xfer.PragmaCard{Name: horizonMergeBasePragma, Values: append(q.hashes(), base)})
	return nil
}

// originMergeBase is the base of q, Fossil's pivot of its primary over its
// secondaries (merge.FindPivot), bounded by limit: the base's hash, or
// noMergeBase when they share no ancestor. ok is false when any named
// check-in is not one the repository holds, or the walk reaches one it does
// not hold: a store answers only from history it holds, since - would be a
// claim about history it cannot see.
func originMergeBase(ctx context.Context, r *repo.Repo, q MergeBaseQuery, limit int) (string, bool, error) {
	if ctx == nil || r == nil {
		panic("sync.originMergeBase: nil context or repo")
	}
	if !q.valid() {
		panic("sync.originMergeBase: malformed query")
	}
	primary, ok, err := heldCheckin(r, q.Primary)
	if err != nil || !ok {
		return "", false, err
	}
	var secondaries []libfossil.FslID
	for _, s := range q.Secondaries {
		rid, ok, err := heldCheckin(r, s)
		if err != nil || !ok {
			return "", false, err
		}
		if rid != primary {
			secondaries = append(secondaries, rid)
		}
	}
	base := primary
	if len(secondaries) == len(q.Secondaries) {
		p, err := merge.FindPivot(ctx, r, merge.PivotQuery{Primary: primary, Secondaries: secondaries, Limit: limit})
		if p.Unheld {
			return "", false, nil
		}
		if errors.Is(err, merge.ErrNoCommonAncestor) {
			return noMergeBase, true, nil
		}
		if err != nil {
			return "", false, err
		}
		base = p.RID
	}
	var uuid string
	err = r.DB().QueryRow(`SELECT uuid FROM blob WHERE rid=? AND size>=0
		AND EXISTS(SELECT 1 FROM event WHERE objid=blob.rid AND type='ci')`, base).Scan(&uuid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sync: merge base: %w", err)
	}
	return uuid, true, nil
}

// heldCheckin resolves a full hash to a check-in the repository holds.
func heldCheckin(r *repo.Repo, uuid string) (libfossil.FslID, bool, error) {
	var rid libfossil.FslID
	err := r.DB().QueryRow(`SELECT rid FROM blob WHERE uuid=? AND size>=0
		AND EXISTS(SELECT 1 FROM event WHERE objid=blob.rid AND type='ci')`, strings.ToLower(uuid)).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("sync: merge base: %w", err)
	}
	return rid, true, nil
}

// mergeBaseAnswer reads an answer to the query asked: the base's hash, or ""
// when the origin names no common ancestor. ok is false for any other pragma
// of the name, one whose hashes do not repeat the query in order among them,
// which is ignored.
func mergeBaseAnswer(p *xfer.PragmaCard, asked MergeBaseQuery) (string, bool) {
	want := asked.hashes()
	if len(p.Values) != len(want)+1 {
		return "", false
	}
	for i, h := range want {
		if !strings.EqualFold(p.Values[i], h) {
			return "", false
		}
	}
	base := p.Values[len(want)]
	if base == noMergeBase {
		return "", true
	}
	if !hash.IsValidHash(strings.ToLower(base)) {
		return "", false
	}
	return strings.ToLower(base), true
}
