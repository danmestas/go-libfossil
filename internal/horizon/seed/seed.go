// Package seed mints, holds and applies the seed of draft-fossil-horizon-00
// §5.2: the control artifact an origin mints so that a structural-horizon
// clone, which lacks C's ancestry, derives the branch, tag and closed state
// of every check-in in the held set K exactly as a dense repository does.
//
// An origin mints with Mint. A clone holds the seed with Hold, outside the
// artifact store (§5.2.3's second holding): it is in no blob row, so nothing
// that enumerates or serves artifacts -- unsent, unclustered, private, a
// gimme, a rebuild's re-marking -- can reach it. The derived state is
// persisted, so where zit applies its seed at every open this package applies
// it whenever the derivation runs: when the seed is held, by Rebuild, and by
// Apply, which the sync layer calls after every round that links artifacts so
// an entry arriving after its seed is seeded too.
package seed

import (
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/leaf"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/tag"
)

var (
	// ErrNotSeed is returned by Hold for bytes that do not hash to the name
	// they were announced under or do not parse as a control artifact with a
	// D card and T cards (§6.2.3), and by Apply for a held seed that no
	// longer parses.
	ErrNotSeed = errors.New("horizon seed: not a seed")
	// ErrProvenance is returned by Hold when the origin's key does not verify
	// the seed (§5.2.4); the clone falls back to the content horizon.
	ErrProvenance = errors.New("horizon seed: provenance does not verify")
	// ErrNothingToSeed is returned by Mint when no tag is in force at any
	// entry, so there is no control artifact to mint; the origin then serves
	// the request as content (§6.2.5).
	ErrNothingToSeed = errors.New("horizon seed: no tag in force at any entry")
)

// Verifier checks a seed against its origin's public key (§5.2.4): nil when
// the seed carries a valid clear-signature by that key. A clone that knows
// no key for its origin passes a nil Verifier and accepts an unsigned seed.
// This package verifies no signature itself: go-libfossil strips a clear-
// signing envelope when it parses (deck.Parse) but implements no signing.
type Verifier func(seed []byte) error

// heldSeedSchema holds the seed in a table of its own, created when a seed is
// held and dropped when it is discarded, like the store cone. It is no
// derived table, so a rebuild keeps it and re-applies it.
const heldSeedSchema = `CREATE TABLE IF NOT EXISTS horizonseed(
  id INTEGER PRIMARY KEY CHECK(id=1),
  seed BLOB NOT NULL
)`

// Entries returns the entries of K (§5.2.1), ascending by rid: C, and every
// check-in of K that names a primary parent outside K. Tag state reaches a
// check-in along its primary parent, so these are where K's state enters
// from below the horizon. A check-in of K other than C that names no parent
// inherits nothing and is no entry, as in zit (src/sparse.zig).
func Entries(q db.Querier, c libfossil.FslID, k []libfossil.FslID) ([]libfossil.FslID, error) {
	if q == nil {
		panic("seed.Entries: q must not be nil")
	}
	if c <= 0 {
		panic("seed.Entries: c must be positive")
	}
	inK := make(map[libfossil.FslID]bool, len(k))
	for _, rid := range k {
		inK[rid] = true
	}
	if !inK[c] {
		return nil, fmt.Errorf("seed.Entries: C rid=%d is not in K", c)
	}
	ids, err := json.Marshal(k)
	if err != nil {
		return nil, fmt.Errorf("seed.Entries: %w", err)
	}
	rows, err := q.Query(`SELECT cid, pid FROM plink
		WHERE isprim=1 AND cid IN (SELECT value FROM json_each(?))`, string(ids))
	if err != nil {
		return nil, fmt.Errorf("seed.Entries: reading plink: %w", err)
	}
	defer rows.Close()
	entries := []libfossil.FslID{c}
	for rows.Next() {
		var cid, pid libfossil.FslID
		if err := rows.Scan(&cid, &pid); err != nil {
			return nil, fmt.Errorf("seed.Entries: reading plink: %w", err)
		}
		if cid != c && !inK[pid] {
			entries = append(entries, cid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("seed.Entries: reading plink: %w", err)
	}
	slices.Sort(entries)
	return slices.Compact(entries), nil
}

// Mint returns the seed for C and its held set k (horizon.HeldSet), with the
// SHA3-256 name it is announced under (§6.2.3). It carries one T card per
// tag in force at each entry in this repository's derivation, applied to the
// entry directly: a propagating row as a `*` card, a singleton as `+`, each
// with the value tagxref holds. Cancel rows are not in force and are left
// out, as zit leaves them out (src/sync.zig composeSeed).
//
// The D card is the latest mtime among the rows the seed summarizes, not the
// time of minting (§6.2.3's SHOULD, which zit follows): two mints over
// unchanged state then yield one name. A fractional latest mtime is rounded
// up to the next whole second, so D is never earlier than the latest event
// (§5.2.2); zit truncates instead. The U card is login, the origin's.
func Mint(q db.Querier, c libfossil.FslID, k []libfossil.FslID, login string) (string, []byte, error) {
	if q == nil {
		panic("seed.Mint: q must not be nil")
	}
	if login == "" {
		panic("seed.Mint: login must not be empty")
	}
	entries, err := Entries(q, c, k)
	if err != nil {
		return "", nil, err
	}
	var cards []deck.TagCard
	var latest int64
	for _, rid := range entries {
		rowCards, rowLatest, err := cardsAt(q, rid)
		if err != nil {
			return "", nil, err
		}
		cards = append(cards, rowCards...)
		latest = max(latest, rowLatest)
	}
	if len(cards) == 0 {
		return "", nil, ErrNothingToSeed
	}
	const second = 1000
	date := time.UnixMilli((latest + second - 1) / second * second).UTC()
	// deck.Marshal writes D to the millisecond; the seed's D is written to
	// the second, as zit writes it, so a seed minted over whole-second tag
	// events is byte-identical to zit's. The cards after D are Marshal's,
	// its Z card replaced by one over the body with D in front.
	tail, err := (&deck.Deck{Type: deck.Control, T: cards, U: deck.User(login)}).Marshal()
	if err != nil {
		return "", nil, fmt.Errorf("seed.Mint: %w", err)
	}
	z := bytes.LastIndex(tail, []byte("Z "))
	body := "D " + date.Format("2006-01-02T15:04:05") + "\n" + string(tail[:z])
	sum := md5.Sum([]byte(body))
	data := []byte(body + "Z " + hex.EncodeToString(sum[:]) + "\n")
	if d, err := parse(data); err != nil || !d.D.Equal(date) {
		panic(fmt.Sprintf("seed.Mint: postcondition: minted seed does not parse back (%v)", err))
	}
	return hash.SHA3(data), data, nil
}

// cardsAt returns a T card for every tag in force at rid, applied to rid,
// and the latest of their mtimes in milliseconds since the Unix epoch.
func cardsAt(q db.Querier, rid libfossil.FslID) ([]deck.TagCard, int64, error) {
	var uuid string
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		return nil, 0, fmt.Errorf("seed.Mint: entry rid=%d: %w", rid, err)
	}
	rows, err := q.Query(`SELECT tag.tagname, tagxref.tagtype, tagxref.value, tagxref.mtime
		FROM tagxref JOIN tag USING(tagid)
		WHERE tagxref.rid=? AND tagxref.tagtype>0`, rid)
	if err != nil {
		return nil, 0, fmt.Errorf("seed.Mint: tags of rid=%d: %w", rid, err)
	}
	defer rows.Close()
	var cards []deck.TagCard
	var latest int64
	for rows.Next() {
		var name string
		var typ int
		var value sql.NullString
		var mtimeRaw any
		if err := rows.Scan(&name, &typ, &value, &mtimeRaw); err != nil {
			return nil, 0, fmt.Errorf("seed.Mint: tags of rid=%d: %w", rid, err)
		}
		mtime, ok := db.ScanJulianDay(mtimeRaw)
		if !ok {
			return nil, 0, fmt.Errorf("seed.Mint: tag %q of rid=%d: unreadable mtime %T", name, rid, mtimeRaw)
		}
		cardType := deck.TagSingleton
		if typ == tag.TagPropagating {
			cardType = deck.TagPropagating
		}
		cards = append(cards, deck.TagCard{Type: cardType, Name: name, UUID: uuid, Value: value.String})
		latest = max(latest, libfossil.MillisFromJulianDay(mtime))
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("seed.Mint: tags of rid=%d: %w", rid, err)
	}
	return cards, latest, nil
}

// Hold makes data, announced under name, the seed this clone holds, and
// applies it. The bytes must hash to name and parse as a control artifact
// (§6.2.3), or ErrNotSeed is returned and nothing changes; verify, when the
// clone knows its origin's key, must accept them (§5.2.4), or ErrProvenance.
//
// A seed replacing a different one (a re-seed, §5.6) re-derives the whole
// store before applying the new seed: the old seed's rows sit in tagxref
// beside rows real artifacts wrote, and only a re-derivation tells them
// apart, which is what zit gets by deriving afresh at every open. Holding the
// seed already held only applies it again.
//
// Hold is for the seed of the clone's own structural request. The sync layer
// routes a file card to it only when a horizon-seed pragma in the same reply
// named it; an origin never calls it (§5.2.3: it refuses a seed offered to
// it), and an artifact of the seed's shape arriving any other way is stored
// and crosslinked as the ordinary control artifact it is.
func Hold(tx *db.Tx, name string, data []byte, verify Verifier) error {
	if tx == nil {
		panic("seed.Hold: tx must not be nil")
	}
	if len(data) == 0 || name == "" || !hash.IsValidHash(name) || hash.ContentHash(data, name) != name {
		return fmt.Errorf("%w: the bytes do not hash to %q", ErrNotSeed, name)
	}
	if _, err := parse(data); err != nil {
		return err
	}
	if verify != nil {
		if err := verify(data); err != nil {
			return fmt.Errorf("%w: %w", ErrProvenance, err)
		}
	}
	old, err := Held(tx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(heldSeedSchema); err != nil {
		return fmt.Errorf("seed.Hold: %w", err)
	}
	if _, err := tx.Exec("REPLACE INTO horizonseed(id, seed) VALUES(1, ?)", data); err != nil {
		return fmt.Errorf("seed.Hold: %w", err)
	}
	if old != nil && !bytes.Equal(old, data) {
		if _, err := manifest.Rederive(tx); err != nil {
			return fmt.Errorf("seed.Hold: %w", err)
		}
	}
	applied, err := Apply(tx, nil)
	if err != nil {
		return err
	}
	if !applied {
		panic("seed.Hold: postcondition: the seed just held was not applied")
	}
	return nil
}

// Held returns the seed this clone holds, or nil when it holds none.
func Held(q db.Querier) ([]byte, error) {
	if q == nil {
		panic("seed.Held: q must not be nil")
	}
	var n int
	if err := q.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='horizonseed'",
	).Scan(&n); err != nil {
		return nil, fmt.Errorf("seed.Held: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	var data []byte
	err := q.QueryRow("SELECT seed FROM horizonseed WHERE id=1").Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("seed.Held: %w", err)
	}
	return data, nil
}

// Apply applies the held seed to the derived state as crosslinking a held
// control artifact would (§5.2.3): each T card at the D card's time, on the
// check-in it names, propagating from there, with the leaf rechecks that
// schedules. It reports whether a seed is held and applied.
//
// Each row it writes names its target as its source, as a check-in's own
// inline card does: the seed is no blob, and a positive source keeps a date
// tag in force. A card whose target this store has not linked yet is
// skipped: held and unlinked, a row sourced from it would read as its own
// link and keep the crosslink from ever linking it. The sync layer calls
// Apply again after each round that links artifacts, and a card already
// applied is superseded by its own row, so a repeat costs a lookup per card.
//
// verify, when the clone knows its origin's key, re-checks the held seed
// (§5.2.4: every seed it holds); a seed that fails is discarded, the store
// re-derived without it, and false returned, so the caller falls back to the
// content horizon.
func Apply(tx *db.Tx, verify Verifier) (bool, error) {
	if tx == nil {
		panic("seed.Apply: tx must not be nil")
	}
	data, err := Held(tx)
	if err != nil || data == nil {
		return false, err
	}
	if verify != nil && verify(data) != nil {
		return false, Discard(tx)
	}
	d, err := parse(data)
	if err != nil {
		return false, err
	}
	mtime := libfossil.TimeToJulian(d.D)
	leaves := leaf.NewPending()
	for _, tc := range d.T {
		var target libfossil.FslID
		err := tx.QueryRow(`SELECT rid FROM blob WHERE uuid=? AND size>=0
			AND EXISTS(SELECT 1 FROM event WHERE objid=blob.rid)`, tc.UUID).Scan(&target)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("seed.Apply: target %s: %w", tc.UUID, err)
		}
		if err := tag.ApplyTagWithTx(tx, tag.ApplyOpts{
			TargetRID: target,
			SrcRID:    target,
			TagName:   tc.Name,
			TagType:   tagType(tc.Type),
			Value:     tc.Value,
			MTime:     mtime,
			Leaves:    leaves,
		}); err != nil {
			return false, fmt.Errorf("seed.Apply: tag %q on %s: %w", tc.Name, tc.UUID, err)
		}
	}
	if err := leaves.Apply(tx); err != nil {
		return false, fmt.Errorf("seed.Apply: %w", err)
	}
	return true, nil
}

// Discard drops the held seed and re-derives the store without it. A
// deepening clone discards the seed for C before it applies the seed for C'
// (§5.8); the seed is local, so this has no synchronization consequence.
func Discard(tx *db.Tx) error {
	if tx == nil {
		panic("seed.Discard: tx must not be nil")
	}
	old, err := Held(tx)
	if err != nil || old == nil {
		return err
	}
	if _, err := tx.Exec("DROP TABLE horizonseed"); err != nil {
		return fmt.Errorf("seed.Discard: %w", err)
	}
	if _, err := manifest.Rederive(tx); err != nil {
		return fmt.Errorf("seed.Discard: %w", err)
	}
	return nil
}

// parse reads data as a seed: a control artifact with a D card and at least
// one T card. deck.Parse strips a clear-signing envelope first.
func parse(data []byte) (*deck.Deck, error) {
	d, err := deck.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSeed, err)
	}
	if d.Type != deck.Control || d.D.IsZero() || len(d.T) == 0 {
		return nil, fmt.Errorf("%w: not a control artifact with D and T cards", ErrNotSeed)
	}
	return d, nil
}

func tagType(t deck.TagType) int {
	switch t {
	case deck.TagPropagating:
		return tag.TagPropagating
	case deck.TagSingleton:
		return tag.TagSingleton
	case deck.TagCancel:
		return tag.TagCancel
	}
	panic(fmt.Sprintf("seed.tagType: unknown tag type %q", t))
}
