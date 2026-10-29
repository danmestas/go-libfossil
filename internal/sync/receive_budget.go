package sync

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
)

// overBudget reports the refusal text for an artifact whose stored form
// would exceed the handler's MaxStoredBytes, "" when it fits, there is no
// budget, or the store already holds it (nothing would be written). payload
// is a file card's content, a delta when the card names a basis; storedBlob
// is a cfile card's stored form, nil for a file card.
func (h *handler) overBudget(uuid string, payload, storedBlob []byte) (string, error) {
	if h.maxStored < 0 {
		panic("handler.overBudget: maxStored must not be negative")
	}
	if h.maxStored == 0 {
		return "", nil
	}
	if held, err := holdsContent(h.repo.DB(), uuid); err != nil || held {
		return "", err
	}
	n, err := budgetSize(payload, storedBlob, h.maxStored)
	if err != nil {
		return "", err
	}
	if n <= h.maxStored {
		return "", nil
	}
	msg := fmt.Sprintf("artifact %s is %d bytes stored; this repository stores at most %d bytes per artifact",
		uuid, n, h.maxStored)
	// The name and check-in are read from this repository, so only a pusher
	// who may read it learns them (phantomName also skips private check-ins).
	// Today push implies read (auth.CanRead), so this holds by construction.
	if !h.canRead() {
		return msg, nil
	}
	name, checkin, err := phantomName(h.repo.DB(), uuid)
	if err != nil {
		return "", err
	}
	if name != "" {
		msg += fmt.Sprintf(", named %s by check-in %s", name, checkin)
	}
	return msg, nil
}

// budgetSize is the length payload occupies in blob.content: storedBlob's
// when the card carried one, else payload's blob.Compress form. A payload
// that cannot reach limit compressed is not compressed just to measure it,
// and is reported as its upper bound, which is within limit.
func budgetSize(payload, storedBlob []byte, limit int) (int, error) {
	if storedBlob != nil {
		return len(storedBlob), nil
	}
	if bound := compressBound(len(payload)); bound <= limit {
		return bound, nil
	}
	c, err := blob.Compress(payload)
	if err != nil {
		return 0, err
	}
	return len(c), nil
}

// compressBound bounds blob.Compress's output for n input bytes: the 4-byte
// size prefix, zlib's 6 bytes of framing, and deflate's 5-byte header per
// stored block, which is what incompressible input becomes. Blocks are
// counted at a quarter of deflate's 65535-byte maximum, plus 64 bytes, so
// the bound may overstate but never understates: len(payload)+4 alone does,
// for random bytes.
func compressBound(n int) int {
	if n < 0 {
		panic("compressBound: n must not be negative")
	}
	return n + 4 + 6 + 5*(n/16384+2) + 64
}

// holdsContent reports whether uuid is in the store with its content, not
// as a phantom: a card for it then stores nothing.
func holdsContent(q db.Querier, uuid string) (bool, error) {
	var size int64
	err := q.QueryRow("SELECT size FROM blob WHERE uuid=?", uuid).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return size >= 0, nil
}

// phantomName returns a file name and the check-in naming uuid, when a
// public check-in this store already crosslinked lists it, so a refusal can
// say which file is too big; "" when none does. A private check-in is never
// named: the refusal goes to whoever pushed, private capability or not.
func phantomName(q db.Querier, uuid string) (name, checkin string, err error) {
	err = q.QueryRow(`SELECT filename.name, ci.uuid
		FROM blob f
		JOIN mlink ON mlink.fid = f.rid
		JOIN filename ON filename.fnid = mlink.fnid
		JOIN blob ci ON ci.rid = mlink.mid
		WHERE f.uuid = ?
		AND NOT EXISTS(SELECT 1 FROM private WHERE private.rid = mlink.mid)
		ORDER BY mlink.mid LIMIT 1`, uuid).Scan(&name, &checkin)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return name, checkin, err
}
