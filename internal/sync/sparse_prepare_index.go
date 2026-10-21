package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// These deliberately small internal prototype limits reject unsupported
// preparation sizes rather than moving unbounded discovery into a request.
// Slot accounting includes each RID and all three words of every slice header;
// map/parser overhead is separately bounded by artifact/card counts.
const (
	sparsePrepArtifacts = 1 << 18
	sparsePrepAliases   = 1 << 20
	sparsePrepSlots     = 1 << 24
)

func (s *preparedSparseService) prepareIndex(ctx context.Context, bound *repo.Repo) error {
	budget := content.ReferenceBudget{Artifacts: sparsePrepArtifacts,
		StoredBytes: 64 << 20, InflatedBytes: 128 << 20,
		ExpandedBytes: 128 << 20, Cards: sparsePrepAliases}
	var err error
	s.facts, s.prepWork, err = content.LoadConeReferenceFactsBounded(ctx, bound.DB(), budget)
	if err != nil {
		return err
	}
	// Validate all raw root/range/depth relationships off path. This temporary
	// classification is discarded; no cone-keyed logical facts are retained.
	validationCone, err := cone.New([]string{"_sparse_validation"}, nil)
	if err != nil {
		panic("sync.prepareIndex: invalid constant validation cone")
	}
	if _, err := s.facts.Classify(ctx, validationCone); err != nil {
		return err
	}
	rows, err := bound.DB().Query(`SELECT b.rid FROM blob b WHERE b.size>=0
		AND NOT EXISTS(SELECT 1 FROM private p WHERE p.rid=b.rid)
		AND NOT EXISTS(SELECT 1 FROM shun s WHERE s.uuid=b.uuid) ORDER BY b.rid`)
	if err != nil {
		return err
	}
	visible := make([]libfossil.FslID, 0)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, rows.Close())
		}
		if len(visible) >= sparsePrepArtifacts {
			return errors.Join(fmt.Errorf("sparse preparation: visibility budget exceeded"), rows.Close())
		}
		var rid libfossil.FslID
		if err := rows.Scan(&rid); err != nil {
			return errors.Join(err, rows.Close())
		}
		visible = append(visible, rid)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	s.index, err = content.NewConeSuccessors(ctx, s.facts, visible, sparsePrepAliases, sparsePrepSlots)
	return err
}
