package libfossil

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/repo"
	internalsync "github.com/danmestas/go-libfossil/internal/sync"
)

// SettingPartialMirror names the boolean repository setting, read as fossil
// reads one ("on", "yes", "true" or a nonzero number), that lets a sparse store
// serve a clone its cone does not cover (draft-fossil-sparse-cone-00 §5.3).
// Unset, such a clone is refused with one error card and no content, since
// the clone would receive elided phantoms it must resolve elsewhere. It
// governs every serving surface: HandleSync, XferHandler and ServeHTTP.
const SettingPartialMirror = internalsync.SettingPartialMirror

// SettingHorizonHistoryURL names the repository setting an origin appends to
// pragma horizon-served as the interface, outside the sync protocol, at
// which history below a horizon can be queried (draft-fossil-horizon-00
// §6.2.5). It must be one http or https URL with a host, no white space and
// no credentials; SetConfig refuses any other value, and an empty one
// names none. Horizon clients report it as CloneResult.HorizonHistoryURL
// and SyncResult.HorizonHistoryURL.
const SettingHorizonHistoryURL = repo.SettingHorizonHistoryURL

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
//
// A cone may carry a horizon (draft-fossil-horizon-00): setting one elides
// blobs only check-ins below it name, and SetStoreCone moves no content, as
// for any narrowing. Setting an older check-in, or nil, widens, and
// SetStoreConeContext fetches what the deeper horizon keeps.
func (r *Repo) SetStoreCone(c *Cone) error {
	if err := content.SetStoreCone(r.inner.DB(), unwrapCone(c)); err != nil {
		return fmt.Errorf("libfossil: set store cone: %w", err)
	}
	return nil
}

// ErrNoStoreCone is returned by CompactStore on a dense store: with no cone
// on record nothing is elided, and compaction never guesses a policy.
var ErrNoStoreCone = errors.New("libfossil: the store has no cone")

// CompactStore is the narrow-then-compact deletion (§5.4), the only one the
// sparse draft defines: it turns held blobs that only out-of-cone F cards name
// into elided phantoms and returns how many it dropped. Structural artifacts,
// sub-manifest nodes whose cards prove an elision, unsent or private content
// and the delta bases of retained artifacts stay held. A later widening
// fetches the dropped blobs again; it is not reversible locally.
func (r *Repo) CompactStore(ctx context.Context) (int, error) {
	if ctx == nil {
		panic("libfossil.Repo.CompactStore: nil context")
	}
	c, err := content.StoreCone(r.inner.DB())
	if err != nil {
		return 0, fmt.Errorf("libfossil: compact store: %w", err)
	}
	if c == nil {
		return 0, ErrNoStoreCone
	}
	n, err := content.CompactStore(ctx, r.inner.DB())
	if err != nil {
		return 0, fmt.Errorf("libfossil: compact store: %w", err)
	}
	return n, nil
}
