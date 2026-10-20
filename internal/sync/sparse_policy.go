package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// sparseKeep is request-local. Unknown non-file artifacts remain dense; a file
// shared by covered and uncovered names is kept under either name.
type sparseKeep struct {
	cone  *cone.Cone
	out   map[libfossil.FslID]bool
	in    map[libfossil.FslID]bool
	nodes map[string]bool
}

func newSparseKeep(ctx context.Context, q db.Querier, c *cone.Cone) (*sparseKeep, error) {
	if c == nil {
		return nil, nil
	}
	k := &sparseKeep{cone: c, out: map[libfossil.FslID]bool{}, in: map[libfossil.FslID]bool{}, nodes: map[string]bool{}}
	rows, err := q.Query(`SELECT m.fid,f.name FROM mlink m JOIN filename f ON f.fnid=m.fnid WHERE m.fid>0
 UNION SELECT m.pid,f.name FROM mlink m JOIN filename f ON f.fnid=m.fnid WHERE m.pid>0`)
	if err != nil {
		return nil, fmt.Errorf("sparse references: %w", err)
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var rid libfossil.FslID
		var name string
		if err := rows.Scan(&rid, &name); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if c.Covers(name) {
			k.in[rid] = true
		} else {
			k.out[rid] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return k, nil
}

func (h *handler) ensureSparseKeep() error {
	if h.cone == nil || h.keep != nil {
		return nil
	}
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	h.keep, err = newSparseKeep(ctx, h.repo.DB(), h.cone)
	return err
}

func (k *sparseKeep) keeps(rid libfossil.FslID) bool {
	return k == nil || !k.out[rid] || k.in[rid]
}

func coneContains(have, want *cone.Cone) bool {
	if have == nil {
		return true
	}
	if want == nil {
		return false
	}
	// Widening beneath an excluded interval cannot be certified by the root
	// member alone. Probe every policy boundary, including re-inclusions.
	for _, m := range want.Include() {
		if !have.Covers(m) {
			return false
		}
	}
	for _, m := range have.Exclude() {
		if want.Covers(m) {
			return false
		}
	}
	return true
}

// localConePragmas reads policy at request time so a widening affects the
// ordinary synchronization path rather than needing a second transfer protocol.
func localConePragmas(q db.Querier) ([]xfer.Card, error) {
	c, err := content.StoreCone(q)
	if err != nil {
		return nil, err
	}
	return conePragmas(c), nil
}
