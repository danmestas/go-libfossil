package sync

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// cloneResume is the walk state a cookie echoes: the row the round starts at
// (aware), for a check-in whose walk outgrew a round, the last rid of that
// walk already sent (set), and the delta bases earlier rounds sent ahead of
// the rows or walks that reach them (runs).
type cloneResume struct {
	row, last  int
	aware, set bool
	runs       []cloneRunRef
}

func cloneResumeFromCards(cards []xfer.Card) cloneResume {
	var resume cloneResume
	for _, card := range cards {
		c, ok := card.(*xfer.CookieCard)
		if !ok {
			continue
		}
		if fields, carried, ok := cloneCapFields(c.Value); ok {
			resume = cloneResume{}
			if len(fields) >= 2 {
				resume = cloneResume{row: fields[1], aware: true, runs: parseCloneRuns(carried)}
			}
			if len(fields) == 3 {
				resume.last, resume.set = fields[2], true
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

// emitCloneResume sends a sparse walk's single cookie: the snapshot bound
// with the row the next round starts at, a part-walked check-in's progress,
// and the delta bases sent ahead of what reaches them. A client echoing the
// cookie verbatim carries all three back.
func (h *handler) emitCloneResume(round *cloneBatch) {
	if h.keep == nil || h.keep.walk == nil {
		return
	}
	value := fmt.Sprintf("%s%d", cloneCapPrefix, h.cloneSnapMax)
	switch {
	case round.pending.set:
		value = fmt.Sprintf("%s:%d:%d", value, round.pending.row, round.pending.last)
	case round.nextRID > 0:
		value = fmt.Sprintf("%s:%d", value, round.nextRID)
	}
	if runs := round.carriedRuns(); round.nextRID > 0 && len(runs) > 0 {
		value += "/" + formatCloneRuns(runs)
	}
	h.resp = append(h.resp, &xfer.CookieCard{Value: value})
}

// cloneSparseRow is §6.2 for one row: content and sub-manifest nodes wait
// for the check-in walk that reaches them in the cone, and are charged here
// as an excluded row is, as is a cluster under a horizon; every other
// artifact goes out at its row, followed by what its walk reaches, each
// checked against the round's budget. A row or item already sent, in this
// round or as a delta base an earlier round carried, is not sent again.
func (h *handler) cloneSparseRow(round *cloneBatch, c cloneCandidate) (bool, error) {
	rid, uuid, fullSize := c.rid, c.uuid, c.fullSize
	w := h.keep.walk
	kind, d := w.classify(rid)
	already := round.sent[rid]
	if !w.atRow(kind) || w.unreached(kind, uuid) {
		if !already {
			round.bytesSent += excludedCost(c)
		}
		return true, nil
	}
	if already {
		round.reached[rid] = true
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
	items, err := w.items(rid, d)
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
		if item <= last {
			continue
		}
		if round.sent[item] {
			round.reached[item] = true
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
			return false, h.cloneWalkFull(round, row, last)
		}
		last = item
	}
	return true, nil
}

// cloneWalkFull ends a round inside row's walk. A request that started at row
// without echoing the walk's state comes from a client that drops it: it
// would return here forever, so the cursor steps past row and what the walk
// still owed reaches it as transit phantoms its next pull names (§6.4).
func (h *handler) cloneWalkFull(round *cloneBatch, row, last int) error {
	if row < round.entry {
		panic("sync.cloneWalkFull: walk row below the round's cursor")
	}
	if row == round.entry && round.resume.row != row {
		round.nextRID = row + 1
		return nil
	}
	round.nextRID = row
	round.pending = cloneResume{row: row, last: last, aware: true, set: true}
	return nil
}
