// Package seedcheck keeps a store from linking or serving a horizon seed it
// did not receive through a horizon-seed announcement
// (draft-fossil-horizon-00 §5.2.3). The receive paths refuse a seed they can
// read as it arrives, so it is never stored, and the link step refuses any
// seed-shaped artifact it reaches (Judge), which then stays stored but is
// never linked or served; what follows is the bookkeeping for a delta that
// cannot be judged yet.
//
// A control artifact is a seed when it carries this implementation's seed
// label or a horizon-seed T card. It is judged when it first becomes
// readable. A receive path judges what it can read at once: the push
// handler refuses the push, a pull or clone drops the artifact. A delta
// whose basis a push or pull lacks is recorded pending, under the root
// of its basis chain (the row whose arrival makes it readable), and withheld
// from every reply until judged. Whatever later fills that root (a push or
// pull, a check-in, a tag) judges the rows pending under it, and only those
// (JudgeFilled); a delta received onto the root, by any path, judges them
// if it is readable and otherwise moves them to its own root (DeltaStored);
// the link step judges a pending row it reaches too (Judge).
// A seed found that way is refused by its hash and dropped: each delta built
// on it is rewritten as full text, so it stays readable, and the seed's row
// keeps its rid but loses its content, out of the phantom table so no
// request asks for it. It is never linked, announced or sent, and a re-push
// of it is refused on its bytes. A pull or clone skips the refused hash.
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
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/delta"
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

// noRoot is the root of a row whose basis chain loops or runs past
// content.MaxDeltaChainDepth: no fill can make it readable, so it stays
// pending, withheld, and is never judged.
const noRoot libfossil.FslID = 0

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
// of its basis chain, for judging when that root is filled.
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
	if _, err := q.Exec("INSERT OR REPLACE INTO "+pendingTable+"(rid, root) VALUES(?, ?)", rid, root); err != nil {
		return fmt.Errorf("seedcheck.MarkPending: %w", err)
	}
	// It leaves the unclustered table too, so neither an inventory nor a
	// cluster names it until it is judged not a seed (decide).
	if _, err := q.Exec("DELETE FROM unclustered WHERE rid=?", rid); err != nil {
		return fmt.Errorf("seedcheck.MarkPending: %w", err)
	}
	return nil
}

// chainRoot follows delta.srcid from rid to the row with no delta, whose
// arrival makes rid readable, or reports noRoot for a chain that loops or
// runs past content.MaxDeltaChainDepth. The walk counts nodes as content's
// does, so a chain it roots is one content can expand, and a planted chain
// cannot hold the write lock for a walk of every row it holds.
func chainRoot(q db.Querier, rid libfossil.FslID) (libfossil.FslID, error) {
	cur := rid
	for nodes := 1; ; nodes++ {
		if nodes > content.MaxDeltaChainDepth {
			return noRoot, nil
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

// DeltaStored is told of every received delta stored at rid, on any path
// (push, pull, clone): the one event besides a full-text fill that can
// change what the rows pending under rid wait on. If rid is readable now,
// those rows are too, and are judged (JudgeFilled). If it is not, they wait
// on what rid waits on, and move to rid's chain root, so the fill that
// finally makes the chain readable judges them, whatever path it takes.
func DeltaStored(q db.Querier, rid libfossil.FslID) error {
	if q == nil || rid <= 0 {
		panic("seedcheck.DeltaStored: q must not be nil and rid must be positive")
	}
	ok, err := tableExists(q, pendingTable)
	if err != nil || !ok {
		return err
	}
	var waiting int
	if err := q.QueryRow("SELECT count(*) FROM "+pendingTable+" WHERE root=?", rid).Scan(&waiting); err != nil {
		return fmt.Errorf("seedcheck.DeltaStored: %w", err)
	}
	if waiting == 0 {
		return nil
	}
	if content.IsAvailable(q, rid) {
		_, err := JudgeFilled(q, []libfossil.FslID{rid})
		return err
	}
	root, err := chainRoot(q, rid)
	if err != nil {
		return err
	}
	if _, err := q.Exec("UPDATE "+pendingTable+" SET root=? WHERE root=?", root, rid); err != nil {
		return fmt.Errorf("seedcheck.DeltaStored: %w", err)
	}
	return nil
}

// WithholdUnreadable records pending every delta row that cannot be
// expanded, so none is announced or served until it can be (§5.2.3). A
// clone, which serves no one while it runs and so records nothing as it
// receives, calls it once at its end.
func WithholdUnreadable(q db.Querier) error {
	if q == nil {
		panic("seedcheck.WithholdUnreadable: q must not be nil")
	}
	deltas, err := rows(q, "SELECT rid, '' FROM delta ORDER BY rid")
	if err != nil {
		return err
	}
	avail := content.NewAvailabilityCache()
	for _, d := range deltas {
		if avail.Available(q, d.rid) {
			continue
		}
		if err := MarkPending(q, d.rid); err != nil {
			return err
		}
	}
	return nil
}

// JudgeFilled judges the rows pending under each of filled, rids whose full
// content was just stored, and reports how many rows it examined. Each fill
// costs the rows that waited on it, not the whole pending set. Each row is
// readable now and is judged: a seed's hash is refused and its content
// dropped (Drop), and either way the pending row goes. A delta pulled onto a
// pending row's basis and itself unreadable is not marked pending, so the
// rows waiting on it stay pending, withheld, until the link step reaches
// them (Judge); a plain file among them stays withheld.
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
			// A row whose own bytes cannot be read (its delta does not apply,
			// or they are not the artifact its name names) stays pending,
			// withheld, and does not stop the other fills in the batch from
			// being judged. Any other failure, the chain to the filled root
			// included, is a fault in the store, and is reported.
			data, err := cache.Expand(q, w.rid)
			if unreadable(err) {
				continue
			}
			if err != nil {
				return examined, fmt.Errorf("seedcheck.JudgeFilled: rid=%d: %w", w.rid, err)
			}
			if _, err := decide(q, w.rid, w.uuid, IsSeedBytes(data)); err != nil {
				return examined, err
			}
		}
	}
	return examined, nil
}

// unreadable reports whether err from expanding a row says the row cannot be
// read, whatever basis arrives: its chain loops or runs past the depth bound
// (a pusher can plant either under a root an honest push later supplies), a
// delta that does not apply, or bytes that are not the artifact its name
// names.
func unreadable(err error) bool {
	return errors.Is(err, content.ErrHashMismatch) || errors.Is(err, content.ErrChainUnresolvable) ||
		errors.Is(err, delta.ErrInvalidDelta) || errors.Is(err, delta.ErrChecksum)
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
	// Horizon F.2: a store never keeps a seed it did not receive through a
	// horizon-seed announcement, and an announced seed is held outside the
	// artifact store, so a seed-shaped artifact reaching the link step is
	// refused whatever path stored it. This catches a seed completed in a
	// later round of a pull or clone, which no pending row records. It is
	// refused and dropped, as a pending seed is (Drop).
	if IsSeed(d) {
		var uuid string
		if err := q.QueryRow("SELECT uuid FROM blob WHERE rid=?", rid).Scan(&uuid); err != nil {
			return false, fmt.Errorf("seedcheck.Judge: rid=%d: %w", rid, err)
		}
		if !t.Pending {
			return true, Drop(q, rid, uuid)
		}
		return decide(q, rid, uuid, true)
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
		if err := Drop(q, rid, uuid); err != nil {
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

// Drop records the seed stored at rid as refused under uuid, takes it out of
// the unclustered table, so no inventory or cluster names it, and drops its
// content: a store never keeps a seed it did not receive through a
// horizon-seed announcement (§5.2.3). It is all or nothing: a failure
// partway leaves the row, and every delta on it, as they were.
func Drop(q db.Querier, rid libfossil.FslID, uuid string) error {
	if q == nil || rid <= 0 || uuid == "" {
		panic("seedcheck.Drop: q must not be nil, rid must be positive and uuid must not be empty")
	}
	return atomic(q, func(q *db.Tx) error {
		if err := Refuse(q, uuid); err != nil {
			return err
		}
		if _, err := q.Exec("DELETE FROM unclustered WHERE rid=?", rid); err != nil {
			return fmt.Errorf("seedcheck: refuse rid=%d: %w", rid, err)
		}
		return drop(q, rid)
	})
}

// atomic runs fn so that its writes land together or not at all: in a
// transaction of its own on a pool, in a savepoint on a transaction or on a
// pool bound to one. A caller that logs a failure and commits (manifest's
// judgeFilled) then commits none of them.
func atomic(q db.Querier, fn func(tx *db.Tx) error) error {
	switch q := q.(type) {
	case *db.DB:
		return q.WithTx(fn)
	case *db.Tx:
		return q.WithSavepoint(fn)
	}
	panic(fmt.Sprintf("seedcheck: a drop needs a *db.DB or *db.Tx, got %T", q))
}

// drop turns the seed stored at rid into a content-less row: size -1, no
// content and no delta link. The row stays, so its rid is never handed to
// another artifact while a receive pass still tracks rows by it, and it is
// left out of the phantom table, so no request asks for it. Each delta built
// on it is rewritten as full text first and still reads; one that does not
// expand is left as it was, as unreadable as before.
func drop(q db.Querier, rid libfossil.FslID) error {
	kids, err := rows(q, "SELECT rid, '' FROM delta WHERE srcid=? ORDER BY rid", rid)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if err := content.Undelta(q, k.rid); err != nil && !unreadable(err) {
			return fmt.Errorf("seedcheck: drop rid=%d: %w", rid, err)
		}
	}
	if _, err := q.Exec("DELETE FROM delta WHERE rid=?", rid); err != nil {
		return fmt.Errorf("seedcheck: drop rid=%d: %w", rid, err)
	}
	if err := blob.DropStored(q, rid); err != nil {
		return fmt.Errorf("seedcheck: drop rid=%d: %w", rid, err)
	}
	return nil
}

// Refuse records uuid as a refused seed. A row stored under it is withheld
// and never linked; one judged a seed here has its content dropped too.
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

// RefusedUUIDs returns every refused hash. The set is small: it holds only
// seeds offered to this store.
func RefusedUUIDs(q db.Querier) (map[string]bool, error) {
	if q == nil {
		panic("seedcheck.RefusedUUIDs: q must not be nil")
	}
	out := map[string]bool{}
	ok, err := tableExists(q, refusedTable)
	if err != nil || !ok {
		return out, err
	}
	all, err := rows(q, "SELECT 0, uuid FROM "+refusedTable)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		out[r.uuid] = true
	}
	return out, nil
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
