package sync

import (
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	"github.com/danmestas/go-libfossil/internal/shun"
)

// A delta and a dropped seed it is built on (horizon §5.2.3). The seed's
// bytes are the only way such a delta can be read, so a store either reads
// the delta while it has them (storeSeedBasis) or does not keep the delta as
// one at all (onRefusedBasis).

// onRefusedBasis reports whether deltaSrc names a seed this store refused,
// whose content is gone (taken). A delta on it could never be read, so it is
// not stored: uuid becomes a phantom instead, private when the card was
// (§7.4), and is asked for again, which a peer answers with its full text.
// created reports whether this call made that phantom, so a peer resending
// the delta adds nothing new. A shunned uuid is left to the caller, which
// drops it.
func onRefusedBasis(q db.Querier, uuid, deltaSrc string, vis visibility) (taken, created bool, err error) {
	if q == nil || uuid == "" {
		panic("sync.onRefusedBasis: q must not be nil and uuid must not be empty")
	}
	if deltaSrc == "" {
		return false, false, nil
	}
	refused, err := seedcheck.IsRefusedUUID(q, deltaSrc)
	if err != nil || !refused {
		return false, false, err
	}
	if shunned, err := shun.IsShunned(q, uuid); err != nil || shunned {
		return false, false, err
	}
	if _, held := blob.Exists(q, uuid); held {
		return true, false, nil
	}
	rid, err := blob.StorePhantom(q, uuid)
	if err != nil {
		return false, false, fmt.Errorf("sync: delta %s on a refused seed: %w", uuid, err)
	}
	if vis == visibilityPrivate {
		if err := content.MakePrivate(q, int64(rid)); err != nil {
			return false, false, fmt.Errorf("sync: delta %s on a refused seed: %w", uuid, err)
		}
	}
	return true, true, nil
}

// storeSeedBasis is what storeResolvedContent does with a seed's full text
// before refusing it (errSeed). When deltas already stored are built on the
// seed's phantom row, the seed is stored just long enough to check those
// deltas against their names (the pass), judge any pending on it, and
// rewrite them as full text, and is then dropped. Otherwise nothing is
// stored.
func storeSeedBasis(r *repo.Repo, pass *receivePass, uuid string, full []byte,
	linker *manifest.ReceiveLinker) error {
	if r == nil || pass == nil || uuid == "" || full == nil {
		panic("sync.storeSeedBasis: r, pass, uuid and full must be set")
	}
	return withLinkerTx(r, linker, func(tx *db.Tx) error {
		rid, held := blob.Exists(tx, uuid)
		if !held {
			return nil
		}
		var size int64
		var deps int
		err := tx.QueryRow("SELECT size, (SELECT count(*) FROM delta WHERE srcid=?1) FROM blob WHERE rid=?1",
			rid).Scan(&size, &deps)
		if err != nil {
			return fmt.Errorf("sync: seed basis %s: %w", uuid, err)
		}
		if size >= 0 || deps == 0 {
			return nil
		}
		if _, _, _, err := writeResolved(tx, uuid, full, nil); err != nil {
			return err
		}
		if err := pass.verifyFrom(tx, rid); err != nil {
			return err
		}
		if _, err := seedcheck.JudgeFilled(tx, []libfossil.FslID{rid}); err != nil {
			return err
		}
		return seedcheck.Drop(tx, rid, uuid)
	})
}
