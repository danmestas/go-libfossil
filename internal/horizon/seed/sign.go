package seed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	stdsync "sync"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/clearsign"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// VerifierFor returns the Verifier for the key q's horizon-seed-key setting
// names (draft-fossil-horizon-00 §5.2.4), or nil when none is set: a clone
// that knows no key for its origin accepts an unsigned seed.
//
// A seed that verified is remembered by its bytes and the key, so the
// re-check every Apply makes (after each round that links artifacts) costs a
// lookup, not a gpg process.
func VerifierFor(q db.Querier) (Verifier, error) {
	key, err := clearsign.SeedKey(q)
	if err != nil || key == nil {
		return nil, err
	}
	return func(seed []byte) error {
		id := verifiedID(key.String(), seed)
		if verified.has(id) {
			return nil
		}
		if err := key.Verify(context.Background(), seed); err != nil {
			return err
		}
		verified.add(id)
		return nil
	}, nil
}

// verifiedMax bounds the seeds remembered as verified: a process verifies
// one seed per structural store it syncs.
const verifiedMax = 256

var verified = &verifiedSet{ids: map[[32]byte]struct{}{}}

type verifiedSet struct {
	mu  stdsync.Mutex
	ids map[[32]byte]struct{}
}

func verifiedID(key string, seed []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(key))
	h.Write([]byte{0})
	h.Write(seed)
	var id [32]byte
	copy(id[:], h.Sum(nil))
	return id
}

func (v *verifiedSet) has(id [32]byte) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.ids[id]
	return ok
}

func (v *verifiedSet) add(id [32]byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.ids) >= verifiedMax {
		clear(v.ids)
	}
	v.ids[id] = struct{}{}
}

// signedSeedSchema keeps the seeds an origin has signed, by the unsigned
// seed's name and the command that signed it. A PGP signature carries its
// time, so signing the same seed twice gives two names; served from here, a
// seed keeps one name while its tag state is unchanged, and a clone that
// already holds it is not sent it again and re-derives nothing.
const signedSeedSchema = `CREATE TABLE IF NOT EXISTS horizonseedsig(
  unsigned TEXT NOT NULL,
  command TEXT NOT NULL,
  signed BLOB NOT NULL,
  mtime INTEGER NOT NULL,
  PRIMARY KEY(unsigned, command)
)`

// signedSeedsMax bounds the table, least recently served dropped first: one
// row per horizon and tag state an origin serves.
const signedSeedsMax = 1024

// touchAfter is how stale a kept signature's serving time may grow before
// serving it records the time again, so a seed served all day is not evicted
// and a busy origin writes its row at most once in this long.
const touchAfter = time.Hour

// Kept is a signature an origin keeps for a seed: the signed bytes and when
// it last recorded serving them.
type Kept struct {
	Signed []byte
	Served time.Time
}

// Stale reports whether serving k should record the time again.
func (k *Kept) Stale(now time.Time) bool { return now.Sub(k.Served) >= touchAfter }

// KeptSignature returns the signature the origin keeps for the unsigned seed
// named name under command, or nil. It only reads, so an origin can look it
// up before it knows whether the requester may read.
func KeptSignature(q db.Querier, name, command string) (*Kept, error) {
	if q == nil || name == "" || command == "" {
		panic("seed.KeptSignature: nil querier, name or command")
	}
	var n int
	if err := q.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='horizonseedsig'",
	).Scan(&n); err != nil || n == 0 {
		return nil, err
	}
	var signed []byte
	var mtime int64
	err := q.QueryRow("SELECT signed, mtime FROM horizonseedsig WHERE unsigned=? AND command=?",
		name, command).Scan(&signed, &mtime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("seed.KeptSignature: %w", err)
	}
	return &Kept{Signed: signed, Served: time.Unix(mtime, 0)}, nil
}

// Keep records signed as the signature of the unsigned seed named name under
// command, served at now, and returns the signature kept. Unless replace is
// set, one a concurrent request kept first wins, so every request names one
// seed; replace drops a kept signature the origin's own key no longer
// verifies.
func Keep(r *repo.Repo, name, command string, signed []byte, replace bool, now time.Time) ([]byte, error) {
	if r == nil || name == "" || command == "" || len(signed) == 0 {
		panic("seed.Keep: nil repo, name, command or signature")
	}
	insert := "INSERT OR IGNORE"
	if replace {
		insert = "INSERT OR REPLACE"
	}
	var kept []byte
	err := r.WithTx(func(tx *db.Tx) error {
		if _, err := tx.Exec(signedSeedSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(insert+` INTO horizonseedsig(unsigned, command, signed, mtime)
			VALUES(?, ?, ?, ?)`, name, command, signed, now.Unix()); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE horizonseedsig SET mtime=? WHERE unsigned=? AND command=?",
			now.Unix(), name, command); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM horizonseedsig WHERE rowid NOT IN
			(SELECT rowid FROM horizonseedsig ORDER BY mtime DESC, rowid DESC LIMIT ?)`,
			signedSeedsMax); err != nil {
			return err
		}
		return tx.QueryRow("SELECT signed FROM horizonseedsig WHERE unsigned=? AND command=?",
			name, command).Scan(&kept)
	})
	if err != nil {
		return nil, fmt.Errorf("seed.Keep: %w", err)
	}
	return kept, nil
}
