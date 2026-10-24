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
// draft-fossil-repo-state-01 Section 7.3), the primary parents whose tags
// must be propagated again because the batch gave each of them a child, and
// whether it linked anything since its last gate, which owes the reparent pass
// even when it scheduled nothing else. All three describe writes in the
// batch's transactions, so they are marked, rolled back and reset together.
//
// It is not safe for concurrent use; one batch owns one.
type batchSchedule struct {
	leaves  *leaf.Pending
	parents parentSet
	owed    bool
	// seedTable caches, for one transaction, whether the seedcheck table
	// exists (0 not yet asked, 1 absent, 2 present), so linking does not
	// look it up per artifact.
	seedTable int8
}

// linkTx runs link in one transaction on r and, when link linked anything,
// runs the whole repair gate (repairOnTx) before that transaction commits:
// the reparent pass, tag propagation from the parents sched holds, and the
// leaf rechecks sched holds. Fossil's manifest_crosslink_end does the same
// inside the transaction that linked the check-ins (src/manifest.c; its
// leaf_do_pending_checks runs before db_end_transaction). A link and the
// repair it owes therefore commit or roll back together: no committed
// check-in waits on a later gate for its parent's tags (issue #432) or its
// leaf row (draft-fossil-repo-state-01 Section 7.3). On commit sched is
// spent; on rollback everything this transaction scheduled is dropped.
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
		if !sched.owed {
			return nil // linked nothing: no gate is owed
		}
		return repairOnTx(tx, sched)
	})
	if err != nil {
		sched.rollback(mark)
		return err
	}
	sched.reset()
	return nil
}

// scheduleMark is a point a batchSchedule can be rolled back to.
type scheduleMark struct {
	leaves  leaf.Mark
	parents int
	owed    bool
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
	return scheduleMark{leaves: s.leaves.Mark(), parents: len(s.parents.order), owed: s.owed}
}

// rollback forgets everything scheduled since m.
func (s *batchSchedule) rollback(m scheduleMark) {
	s.leaves.Rollback(m.leaves)
	s.parents.truncate(m.parents)
	s.owed = m.owed
	s.seedTable = 0
}

// reset empties the schedule once the gate that ran it has committed.
func (s *batchSchedule) reset() {
	s.leaves.Reset()
	s.parents.truncate(0)
	s.owed = false
	s.seedTable = 0
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
