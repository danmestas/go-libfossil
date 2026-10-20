package libfossil

import (
	"context"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/content"
)

// CompactStore explicitly reclaims held file content outside the store cone
// (draft-fossil-sparse-cone-00 §5.4). It returns the number of artifacts changed
// to recoverable phantoms, preserving their identities for a later pull.
// Setting or narrowing a cone alone never reclaims content.
//
// Spines, control artifacts, tree nodes, shared in-cone content, unpublished or
// private content, and retained delta dependencies stay held. A dense store or
// lost cone is a no-op. Corruption or an incomplete spine aborts compaction.
// All changes run in one write transaction and roll back on error or observed
// cancellation. SQLite's configured busy timeout bounds waiting for its writer
// lock; cancellation is checked before and after that wait, not within it.
// Reclaimed bytes become reusable database space, not a guaranteed smaller file.
func (r *Repo) CompactStore(ctx context.Context) (int, error) {
	count, err := content.CompactStore(ctx, r.inner.DB())
	if err != nil {
		return 0, fmt.Errorf("libfossil: compact store: %w", err)
	}
	return count, nil
}
