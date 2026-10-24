// Package seedcheck keeps an origin from linking or serving a horizon seed
// pushed to it (draft-fossil-horizon-00 §5.2.3).
//
// A pushed control artifact is a seed when it carries this implementation's
// seed label or a horizon-seed T card. It is judged when it first becomes
// readable. The push handler judges what it can read at once, and refuses
// the push. A delta whose basis it lacks is recorded pending, under the root
// of its basis chain (the row whose arrival makes it readable), and withheld
// from every reply until judged. Whatever later fills that root (a push or
// pull, a check-in, a tag) judges the rows pending under it, and only those
// (JudgeFilled); the link step judges a pending row it reaches too (Judge).
// A seed found that way is refused by its hash: it stays stored, so a delta
// built on it stays readable, but it is never linked, announced or sent, and
// a re-push of it is refused on its bytes. Pulling the same hash from a peer
// stores nothing new: the row is held already, and stays refused.
//
// Both tables are store-local: no configuration group carries them, so no
// peer pulls them and a refusal can never make a clone drop the seed it
// holds. Each is created on first use; a store without them withholds
// nothing.
package seedcheck

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
)

// Label is the U card of the seeds go-libfossil mints: its own seed label,
// never a user's login, since a seed is a tag application no user made
// (§5.2.2).
const Label = "go-libfossil"

// Tag is the tag name of a seed's binding card; no pushed control artifact
// may carry it (§5.2.3).
const Tag = "horizon-seed"

const (
	pendingTable = "seedpending" // pushed deltas not yet readable, by rid, under their chain root
	refusedTable = "seedrefused" // hashes of refused seeds
)

const (
	pendingSchema = `CREATE TABLE IF NOT EXISTS ` + pendingTable + `(
  rid INTEGER PRIMARY KEY,
  root INTEGER NOT NULL
)`
	pendingIndex  = `CREATE INDEX IF NOT EXISTS ` + pendingTable + `_root ON ` + pendingTable + `(root)`
	refusedSchema = `CREATE TABLE IF NOT EXISTS ` + refusedTable + `(uuid TEXT PRIMARY KEY) WITHOUT ROWID`
)

// maxChainSteps bounds a basis-chain walk, so a cycle in the delta table is
// an error rather than a hang.
const maxChainSteps = 1 << 24

// judgeCacheBytes bounds the expanded content one JudgeFilled call keeps,
// so a chain pending under one root is rebuilt once along its length.
const judgeCacheBytes = 16 << 20

// IsSeed reports whether d is a seed an origin refuses: a control artifact
// whose U card is Label, or one carrying a Tag T card under any label.
// Another implementation's label alone is ordinary, and so is a user's
// login unless it equals Label.
func IsSeed(d *deck.Deck) bool {
	if d == nil {
		panic("seedcheck.IsSeed: d must not be nil")
	}
	if d.Type != deck.Control {
		return false
	}
	if d.U != nil && *d.U == Label {
		return true
	}
	for _, t := range d.T {
		if t.Name == Tag {
			return true
		}
	}
	return false
}

// IsSeedBytes is IsSeed over an artifact's full content. Bytes that name
// neither the label nor the tag are rejected without parsing.
func IsSeedBytes(data []byte) bool {
	if data == nil {
		panic("seedcheck.IsSeedBytes: data must not be nil")
	}
	if !bytes.Contains(data, []byte("\nU "+Label+"\n")) && !bytes.Contains(data, []byte(Tag)) {
		return false
	}
	d, err := deck.Parse(data)
	return err == nil && IsSeed(d)
}

// MarkPending records rid, a pushed delta not yet readable, under the root
// of its basis chain, for judging when that root is filled. Rows pending
// under rid itself move to the same root: rid, stored as a delta on a basis
// not held either, is no more readable than that basis, and filling a
// phantom with a delta fills nothing JudgeFilled hears of.
func MarkPending(q db.Querier, rid libfossil.FslID) error {
	if q == nil || rid <= 0 {
		panic("seedcheck.MarkPending: q must not be nil and rid must be positive")
	}
	root, err := chainRoot(q, rid)
	if err != nil {
		return err
	}
	return markPending(q, rid, root)
}

func markPending(q db.Querier, rid, root libfossil.FslID) error {
	for _, stmt := range []string{pendingSchema, pendingIndex} {
		if _, err := q.Exec(stmt); err != nil {
			return fmt.Errorf("seedcheck.MarkPending: %w", err)
		}
	}
	for _, stmt := range []string{
		"INSERT OR REPLACE INTO " + pendingTable + "(rid, root) VALUES(?1, ?2)",
		"UPDATE " + pendingTable + " SET root=?2 WHERE root=?1",
	} {
		if _, err := q.Exec(stmt, rid, root); err != nil {
			return fmt.Errorf("seedcheck.MarkPending: %w", err)
		}
	}
	// It leaves the unclustered table too, so neither an inventory nor a
	// cluster names it until it is judged not a seed (decide).
	if _, err := q.Exec("DELETE FROM unclustered WHERE rid=?", rid); err != nil {
		return fmt.Errorf("seedcheck.MarkPending: %w", err)
	}
	return nil
}

// chainRoot follows delta.srcid from rid to the row with no delta, whose
// arrival makes rid readable.
func chainRoot(q db.Querier, rid libfossil.FslID) (libfossil.FslID, error) {
	cur := rid
	for steps := 0; ; steps++ {
		if steps > maxChainSteps {
			return 0, fmt.Errorf("seedcheck: basis chain of rid=%d does not end", rid)
		}
		var next libfossil.FslID
		err := q.QueryRow("SELECT srcid FROM delta WHERE rid=?", cur).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return cur, nil
		}
		if err != nil {
			return 0, fmt.Errorf("seedcheck: basis chain of rid=%d: %w", rid, err)
		}
		cur = next
	}
}

// JudgeFilled judges the rows pending under each of filled, rids whose full
// content was just stored, and reports how many rows it examined. Each fill
// costs the rows that waited on it, not the whole pending set. Each row is
// readable now and is judged: a seed's hash is refused, and either way the
// row goes. A delta pulled onto a pending row's basis and itself unreadable
// is not marked pending, so the rows waiting on it stay pending, withheld,
// until the link step reaches them (Judge); a plain file among them stays
// withheld.
//
// A row that does not expand, or whose bytes do not hash to its name, is
// not the artifact it names; it stays pending, withheld, rather than be
// judged on bytes that are not its own, and the batch goes on.
func JudgeFilled(q db.Querier, filled []libfossil.FslID) (int, error) {
	if q == nil {
		panic("seedcheck.JudgeFilled: q must not be nil")
	}
	ok, err := tableExists(q, pendingTable)
	if err != nil || !ok || len(filled) == 0 {
		return 0, err
	}
	cache := content.NewCache(judgeCacheBytes)
	examined := 0
	for _, root := range filled {
		if root <= 0 {
			panic("seedcheck.JudgeFilled: filled rids must be positive")
		}
		waiting, err := rows(q, "SELECT p.rid, b.uuid FROM "+pendingTable+" p JOIN blob b ON b.rid=p.rid"+
			" WHERE p.root=? ORDER BY p.rid", root)
		if err != nil {
			return examined, err
		}
		if len(waiting) == 0 {
			continue
		}
		for _, w := range waiting {
			examined++
			// A row that does not expand, or whose bytes are not the artifact
			// its name names, stays pending, withheld, and does not stop the
			// other fills in the batch from being judged.
			data, err := cache.Expand(q, w.rid)
			if err != nil {
				continue
			}
			if _, err := decide(q, w.rid, w.uuid, IsSeedBytes(data)); err != nil {
				return examined, err
			}
		}
	}
	return examined, nil
}

// Tables is which of the two tables a store has. A caller that links many
// artifacts reads it once per transaction (Present) and hands it to Judge.
type Tables struct {
	Pending, Refused bool
}

// Present reads which tables exist.
func Present(q db.Querier) (Tables, error) {
	if q == nil {
		panic("seedcheck.Present: q must not be nil")
	}
	var t Tables
	var err error
	if t.Pending, err = tableExists(q, pendingTable); err != nil {
		return Tables{}, err
	}
	t.Refused, err = tableExists(q, refusedTable)
	return t, err
}

// Judge decides rid, about to be linked with d its parsed content, given
// the tables the store has, and reports whether it must not be linked: its
// hash is refused, or it was pending and is a seed, which refuses its hash.
// Any other pending row is cleared. This is the derive-time check every
// link path reaches, behind JudgeFilled.
func Judge(q db.Querier, t Tables, rid libfossil.FslID, d *deck.Deck) (bool, error) {
	if q == nil || rid <= 0 || d == nil {
		panic("seedcheck.Judge: q and d must not be nil and rid must be positive")
	}
	if !t.Pending && !t.Refused {
		return false, nil
	}
	var uuid string
	if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
		return false, fmt.Errorf("seedcheck.Judge: rid=%d: %w", rid, err)
	}
	if t.Refused {
		var n int
		if err := q.QueryRow("SELECT count(*) FROM "+refusedTable+" WHERE uuid=?", uuid).Scan(&n); err != nil {
			return false, fmt.Errorf("seedcheck.Judge: %w", err)
		}
		if n > 0 {
			return true, nil
		}
	}
	if !t.Pending {
		return false, nil
	}
	var one int
	err := q.QueryRow("SELECT 1 FROM "+pendingTable+" WHERE rid=?", rid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("seedcheck.Judge: pending rid=%d: %w", rid, err)
	}
	return decide(q, rid, uuid, IsSeed(d))
}

// decide records the verdict on pending rid, which MarkPending took out of
// the unclustered table: a seed's hash is refused and it stays out, so no
// inventory or cluster names it; anything else goes back in. Either way the
// pending row goes.
func decide(q db.Querier, rid libfossil.FslID, uuid string, seed bool) (bool, error) {
	if seed {
		if err := Refuse(q, uuid); err != nil {
			return false, err
		}
	} else if _, err := q.Exec("INSERT OR IGNORE INTO unclustered(rid) VALUES(?)", rid); err != nil {
		return false, fmt.Errorf("seedcheck: judge rid=%d: %w", rid, err)
	}
	if _, err := q.Exec("DELETE FROM "+pendingTable+" WHERE rid=?", rid); err != nil {
		return false, fmt.Errorf("seedcheck: judge rid=%d: %w", rid, err)
	}
	return seed, nil
}

// Refuse records uuid as a refused seed. The row stored under it is kept,
// so deltas on it stay readable, but it is withheld and never linked.
func Refuse(q db.Querier, uuid string) error {
	if q == nil || uuid == "" {
		panic("seedcheck.Refuse: q must not be nil and uuid must not be empty")
	}
	if _, err := q.Exec(refusedSchema); err != nil {
		return fmt.Errorf("seedcheck.Refuse: %w", err)
	}
	if _, err := q.Exec("INSERT OR IGNORE INTO "+refusedTable+"(uuid) VALUES(?)", uuid); err != nil {
		return fmt.Errorf("seedcheck.Refuse: %w", err)
	}
	return nil
}

// IsRefusedUUID reports whether uuid was refused as a seed.
func IsRefusedUUID(q db.Querier, uuid string) (bool, error) {
	if q == nil {
		panic("seedcheck.IsRefusedUUID: q must not be nil")
	}
	ok, err := tableExists(q, refusedTable)
	if err != nil || !ok {
		return false, err
	}
	var n int
	if err := q.QueryRow("SELECT count(*) FROM "+refusedTable+" WHERE uuid=?", uuid).Scan(&n); err != nil {
		return false, fmt.Errorf("seedcheck.IsRefusedUUID: %w", err)
	}
	return n > 0, nil
}

// ForgetDeleted drops the pending rows whose blob rows are gone. shun.Purge,
// the one deleter of blob rows, runs it in the same transaction: rids are
// reused, so a later artifact stored under one must not be withheld.
func ForgetDeleted(q db.Querier) error {
	if q == nil {
		panic("seedcheck.ForgetDeleted: q must not be nil")
	}
	ok, err := tableExists(q, pendingTable)
	if err != nil || !ok {
		return err
	}
	const gone = " FROM " + pendingTable + " WHERE NOT EXISTS (SELECT 1 FROM blob WHERE rid=" + pendingTable + ".rid)"
	var n int
	if err := q.QueryRow("SELECT count(*)" + gone).Scan(&n); err != nil || n == 0 {
		return err
	}
	if _, err := q.Exec("DELETE" + gone); err != nil {
		return fmt.Errorf("seedcheck.ForgetDeleted: %w", err)
	}
	return nil
}

// IsWithheld reports whether rid is pending, or stored under a refused
// hash. No reply announces or sends it.
func IsWithheld(q db.Querier, rid libfossil.FslID) (bool, error) {
	if q == nil {
		panic("seedcheck.IsWithheld: q must not be nil")
	}
	t, err := Present(q)
	if err != nil {
		return false, err
	}
	var n int
	if t.Pending {
		if err := q.QueryRow("SELECT count(*) FROM "+pendingTable+" WHERE rid=?", rid).Scan(&n); err != nil {
			return false, fmt.Errorf("seedcheck.IsWithheld: %w", err)
		}
	}
	if n == 0 && t.Refused {
		if err := q.QueryRow("SELECT count(*) FROM blob b JOIN "+refusedTable+" r ON r.uuid=b.uuid WHERE b.rid=?",
			rid).Scan(&n); err != nil {
			return false, fmt.Errorf("seedcheck.IsWithheld: %w", err)
		}
	}
	return n > 0, nil
}

// Withheld returns every rid IsWithheld reports. The set is small: it holds
// only pushed deltas not yet readable, and refused seeds.
func Withheld(q db.Querier) (map[libfossil.FslID]bool, error) {
	if q == nil {
		panic("seedcheck.Withheld: q must not be nil")
	}
	t, err := Present(q)
	if err != nil {
		return nil, err
	}
	out := map[libfossil.FslID]bool{}
	var queries []string
	if t.Pending {
		queries = append(queries, "SELECT rid, '' FROM "+pendingTable)
	}
	if t.Refused {
		queries = append(queries, "SELECT b.rid, '' FROM blob b JOIN "+refusedTable+" r ON r.uuid=b.uuid")
	}
	for _, query := range queries {
		found, err := rows(q, query)
		if err != nil {
			return nil, err
		}
		for _, r := range found {
			out[r.rid] = true
		}
	}
	return out, nil
}

func tableExists(q db.Querier, name string) (bool, error) {
	var n int
	if err := q.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).
		Scan(&n); err != nil {
		return false, fmt.Errorf("seedcheck: %w", err)
	}
	return n > 0, nil
}

// row is a rid and, where the query selects one, its hash.
type row struct {
	rid  libfossil.FslID
	uuid string
}

func rows(q db.Querier, query string, args ...any) ([]row, error) {
	rs, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("seedcheck: %w", err)
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.rid, &r.uuid); err != nil {
			return nil, fmt.Errorf("seedcheck: %w", err)
		}
		out = append(out, r)
	}
	return out, rs.Err()
}
