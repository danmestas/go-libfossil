package libfossil

import (
	"fmt"

	"github.com/danmestas/go-libfossil/internal/content"
)

// StoreCone returns the repository's store cone, or nil when the store is
// dense. A store cone (draft-fossil-sparse-cone-00 §5) is local policy: an
// artifact the cone leaves out may be deliberately absent, an elided phantom,
// which synchronization does not request and verification counts apart from
// damage. A cone this version cannot read is lost policy and reads as nil, so
// the store heals toward dense.
func (r *Repo) StoreCone() (*Cone, error) {
	inner, err := content.StoreCone(r.inner.DB())
	if err != nil {
		return nil, fmt.Errorf("libfossil: store cone: %w", err)
	}
	return wrapCone(inner), nil
}

// SetStoreCone replaces the store cone; nil makes the store dense. It moves
// no content. Narrowing deletes nothing (§5.4). Widening makes the newly
// covered elided phantoms wanted again, and the next pull fetches them.
// While a cone is set, a commit carries no R card (§4.3).
func (r *Repo) SetStoreCone(c *Cone) error {
	if err := content.SetStoreCone(r.inner.DB(), unwrapCone(c)); err != nil {
		return fmt.Errorf("libfossil: set store cone: %w", err)
	}
	return nil
}
