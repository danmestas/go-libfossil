package manifest

import (
	"slices"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// batchSchedule is the work a crosslink batch defers to its repair gate: the
// leaf rechecks its branch writes schedule (leaf.Pending,
// draft-fossil-repo-state-01 Section 7.3) and the primary parents whose tags
// must be propagated again because the batch gave each of them a child. Both
// describe writes in the batch's transactions, so they are marked, rolled back
// and reset together.
//
// It is not safe for concurrent use; one batch owns one.
type batchSchedule struct {
	leaves  *leaf.Pending
	parents parentSet
}

// linkTx runs link in one transaction on r and, before that transaction
// commits, reparents and propagates tags from the parents sched holds, as
// fossil's manifest_crosslink propagates from a check-in's parent in the
// transaction that links it. A link and the propagation it owes therefore
// commit or roll back together: no committed plink row waits on a later gate
// for its parent's tags (issue #432). On commit the parents this transaction
// added are dropped, propagated; on rollback everything it scheduled is. The
// leaf rechecks stay on sched for the closing gate, which applies the whole
// batch's at once (draft-fossil-repo-state-01 Section 7.3).
func linkTx(r *repo.Repo, sched *batchSchedule, link func(tx *db.Tx) error) error {
	if r == nil {
		panic("manifest.linkTx: r must not be nil")
	}
	if sched == nil {
		panic("manifest.linkTx: sched must not be nil")
	}
	mark := sched.mark()
	err := r.WithTx(func(tx *db.Tx) error {
		if err := link(tx); err != nil {
			return err
		}
		if len(sched.parents.order) == mark.parents {
			return nil // linked no parent: nothing owes propagation
		}
		return repairPlinkAndTags(tx, sched)
	})
	if err != nil {
		sched.rollback(mark)
		return err
	}
	sched.parents.truncate(mark.parents)
	return nil
}

// scheduleMark is a point a batchSchedule can be rolled back to.
type scheduleMark struct {
	leaves  leaf.Mark
	parents int
}

func newBatchSchedule() *batchSchedule {
	return &batchSchedule{
		leaves:  leaf.NewPending(),
		parents: parentSet{seen: make(map[libfossil.FslID]struct{})},
	}
}

// mark records the schedule's current extent, for a caller whose transaction
// or savepoint may roll back.
func (s *batchSchedule) mark() scheduleMark {
	return scheduleMark{leaves: s.leaves.Mark(), parents: len(s.parents.order)}
}

// rollback forgets everything scheduled since m.
func (s *batchSchedule) rollback(m scheduleMark) {
	s.leaves.Rollback(m.leaves)
	s.parents.truncate(m.parents)
}

// reset empties the schedule once the gate that ran it has committed.
func (s *batchSchedule) reset() {
	s.leaves.Reset()
	s.parents.truncate(0)
}

// parentSet holds the primary parents a batch wrote plink rows for, in the
// order it first recorded them, so a rollback drops exactly what it added.
// They are the parents repairTagPropagation starts from (issue #432).
type parentSet struct {
	seen  map[libfossil.FslID]struct{}
	order []libfossil.FslID
}

// add records pid, a primary parent whose plink row the batch just wrote.
func (p *parentSet) add(pid libfossil.FslID) {
	if pid <= 0 {
		panic("manifest.parentSet.add: pid must be positive")
	}
	if _, ok := p.seen[pid]; ok {
		return
	}
	p.seen[pid] = struct{}{}
	p.order = append(p.order, pid)
}

func (p *parentSet) truncate(n int) {
	if n < 0 || n > len(p.order) {
		panic("manifest.parentSet.truncate: mark is past the set's end")
	}
	for _, pid := range p.order[n:] {
		delete(p.seen, pid)
	}
	p.order = p.order[:n]
}

// sorted returns the parents in ascending rid order, the order the
// whole-repository pass visited them in, so a gate's writes are reproducible.
func (p *parentSet) sorted() []libfossil.FslID {
	out := slices.Clone(p.order)
	slices.Sort(out)
	return out
}
