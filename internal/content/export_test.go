package content

import (
	"context"

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
