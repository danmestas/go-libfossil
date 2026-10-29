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

// TreeBytes is what m charges for the trees it keeps.
func (m *Memo) TreeBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trees == nil {
		return 0
	}
	return m.trees.bytes
}

// TreeChainDepth is the most set differences any tree m keeps lies from
// one kept whole.
func (m *Memo) TreeChainDepth() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	depth := 0
	if m.trees != nil {
		for _, p := range m.trees.pieces {
			depth = max(depth, p.depth)
		}
	}
	return depth
}

// ForgetStore makes m's next view read its store in full, as one does after
// an arrival a held artifact cited, keeping what else m holds.
func (m *Memo) ForgetStore() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = nil
}
