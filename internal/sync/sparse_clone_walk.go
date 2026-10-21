package sync

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// cloneResume names a check-in row whose walk outgrew a round, and the last
// rid of that walk already sent.
type cloneResume struct {
	row, last int
	set       bool
}

func cloneResumeFromCards(cards []xfer.Card) cloneResume {
	var resume cloneResume
	for _, card := range cards {
		c, ok := card.(*xfer.CookieCard)
		if !ok {
			continue
		}
		if fields, ok := cloneCapFields(c.Value); ok {
			resume = cloneResume{}
			if len(fields) == 3 {
				resume = cloneResume{row: fields[1], last: fields[2], set: true}
			}
		}
	}
	return resume
}

func hasPullCard(cards []xfer.Card) bool {
	for _, card := range cards {
		if _, ok := card.(*xfer.PullCard); ok {
			return true
		}
	}
	return false
}

// emitCloneResume hands a part-walked check-in to the next round, or clears
// the progress the request carried once that check-in is done.
func (h *handler) emitCloneResume(round *cloneBatch) {
	if !round.pending.set && !round.resume.set {
		return
	}
	value := fmt.Sprintf("%s%d", cloneCapPrefix, h.cloneSnapMax)
	if round.pending.set {
		value = fmt.Sprintf("%s:%d:%d", value, round.pending.row, round.pending.last)
	}
	h.resp = append(h.resp, &xfer.CookieCard{Value: value})
}

// cloneSparseRow is §6.2 for one row: content and sub-manifest nodes wait
// for the check-in walk that reaches them in the cone, and are charged here
// as an excluded row is; every other artifact goes out at its row, followed
// by what its walk reaches, each checked against the round's budget.
func (h *handler) cloneSparseRow(round *cloneBatch, rid int, uuid string, fullSize int) (bool, error) {
	w := h.keep.walk
	kind, d := w.classify(rid)
	already := round.sent[rid]
	if kind == sparseContent || kind == sparseNode {
		if !already {
			round.bytesSent += cloneCardOverheadBytes + max(fullSize, 0)
		}
		return true, nil
	}
	resuming := round.resume.set && round.resume.row == rid
	if !resuming && !already {
		sent, err := h.cloneSend(round, rid, uuid, fullSize)
		if err != nil || !sent {
			round.nextRID = rid
			return false, err
		}
	}
	if d == nil {
		return true, nil
	}
	items, err := w.items(d)
	if err != nil {
		return false, err
	}
	last := 0
	if resuming {
		last = round.resume.last
	}
	return h.cloneWalkItems(round, rid, items, last)
}

// cloneWalkItems sends a walk's rows above last, ascending. A full round
// leaves the check-in as the cursor and its progress pending.
func (h *handler) cloneWalkItems(round *cloneBatch, row int, items []int, last int) (bool, error) {
	for _, item := range items {
		if item <= last || round.sent[item] {
			continue
		}
		info, ok, err := h.loadCloneBlob(item)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		sent := false
		if round.count == 0 || round.bytesSent < round.budget {
			if sent, err = h.cloneSend(round, item, info.uuid, info.size); err != nil {
				return false, err
			}
		}
		if !sent {
			round.nextRID = row
			round.pending = cloneResume{row: row, last: last, set: true}
			return false, nil
		}
		last = item
	}
	return true, nil
}
