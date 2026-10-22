package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/merge"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// horizonMergeBasePragma asks an origin for the merge base of two check-ins,
// `pragma horizon-mergebase A B`, and carries its answer, `pragma
// horizon-mergebase A B BASE`, BASE being - when the two share no ancestor
// (draft-fossil-horizon-00 §6.2.4).
const horizonMergeBasePragma = "horizon-mergebase"

// noMergeBase is the answer naming no common ancestor.
const noMergeBase = "-"

// mergeBaseNodes bounds the check-ins one merge-base answer visits (§8: the
// bound of the origin's other graph walks, horizon.HeldSet's among them).
const mergeBaseNodes = 1 << 20

// mergeBaseQuery reads a merge-base query: two well-formed hashes, or ok
// false, which leaves the pragma ignored like any it does not know.
func mergeBaseQuery(p *xfer.PragmaCard) ([2]string, bool) {
	if len(p.Values) != 2 || !hash.IsValidHash(p.Values[0]) || !hash.IsValidHash(p.Values[1]) {
		return [2]string{}, false
	}
	return [2]string{p.Values[0], p.Values[1]}, true
}

// answerMergeBase appends the answer to the request's merge-base query, if
// it carried one this origin can answer. It answers in a pull's reply only,
// where zit answers it, so the requester has read access to the history the
// base is drawn from.
func (h *handler) answerMergeBase(ctx context.Context) error {
	q := h.mergeBaseQuery
	if q == [2]string{} || !h.pullOK {
		return nil
	}
	base, ok, err := originMergeBase(ctx, h.repo, q[0], q[1], mergeBaseNodes)
	if errors.Is(err, merge.ErrBound) {
		return nil // unanswered: the client reads it as "deepen by hand"
	}
	if err != nil || !ok {
		return err
	}
	h.resp = append(h.resp, &xfer.PragmaCard{Name: horizonMergeBasePragma, Values: []string{q[0], q[1], base}})
	return nil
}

// originMergeBase is the merge base of the check-ins named a and b by the
// repository's own walk (merge.FindCommonAncestorWithin), bounded by limit:
// the base's hash, or noMergeBase when they share no ancestor. ok is false
// when either is not a check-in the repository holds, or the base it finds
// is not one: a store answers only from history it holds.
func originMergeBase(ctx context.Context, r *repo.Repo, a, b string, limit int) (string, bool, error) {
	if ctx == nil || r == nil {
		panic("sync.originMergeBase: nil context or repo")
	}
	ra, okA, err := heldCheckin(r, a)
	if err != nil || !okA {
		return "", false, err
	}
	rb, okB, err := heldCheckin(r, b)
	if err != nil || !okB {
		return "", false, err
	}
	base, err := merge.FindCommonAncestorWithin(ctx, r, ra, rb, limit)
	if errors.Is(err, merge.ErrNoCommonAncestor) {
		return noMergeBase, true, nil
	}
	if err != nil {
		return "", false, err
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
// of the name, which is ignored.
func mergeBaseAnswer(p *xfer.PragmaCard, asked [2]string) (string, bool) {
	if len(p.Values) != 3 || !strings.EqualFold(p.Values[0], asked[0]) || !strings.EqualFold(p.Values[1], asked[1]) {
		return "", false
	}
	if p.Values[2] == noMergeBase {
		return "", true
	}
	if !hash.IsValidHash(strings.ToLower(p.Values[2])) {
		return "", false
	}
	return strings.ToLower(p.Values[2]), true
}
