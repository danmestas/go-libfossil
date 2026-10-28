package content

import (
	"context"
	"maps"
	"slices"

	"github.com/danmestas/go-libfossil/db"
)

// LoadAbsencesWithin is LoadAbsencesContext under injected budgets, and
// reports what classification read. The tests that use it are an external
// package: their fixtures need the manifest writer, which imports this one.
func LoadAbsencesWithin(
	ctx context.Context, q db.Querier, refs ReferenceBudget, treePerCheckin int64,
) (*Absences, ReferenceWork, error) {
	treeMax := treePerCheckin << 10
	if treeMax > defaultReferenceLimits.treeMax {
		treeMax = defaultReferenceLimits.treeMax
	}
	a, err := loadAbsences(ctx, q, referenceLimits{
		refs: refs, treePerCheckin: treePerCheckin, treeMax: treeMax,
	})
	if a == nil {
		return nil, ReferenceWork{}, err
	}
	return a, a.work, err
}

// LoadAbsencesWork is LoadAbsencesContext, reporting what classification
// read under the default budgets.
func LoadAbsencesWork(ctx context.Context, q db.Querier) (*Absences, ReferenceWork, error) {
	limits := defaultReferenceLimits
	return LoadAbsencesWithin(ctx, q, limits.refs, limits.treePerCheckin)
}

// TreesRead names the trees v's horizon walks read from the store, and
// counts the reads.
func (v *MemoView) TreesRead() (names []string, reads int) {
	return slices.Collect(maps.Keys(v.trees)), v.treeReads
}

// TreeBytes is what m charges for the trees it keeps: its store's tree
// index, or the trees its walks read when the store keeps none.
func (m *Memo) TreeBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store != nil && m.store.trees != nil {
		return m.store.trees.bytes
	}
	return m.trees.bytes
}

// LimitTrees sets the most m's store tree index may keep; past it, m's
// walks read trees as they need them.
func (m *Memo) LimitTrees(bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.treeLimit = bytes
}
