package sync

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// horizonCursorPragma carries the horizon cursor, `pragma horizon-cursor N`
// (draft-fossil-horizon-00 §6.1, §6.2.6): in a reply, the origin's position
// through which it served the request; in a request, the position the
// client was last served through, so the origin announces only what lies
// above it. zit spells and places it the same (sync.zig
// horizon_cursor_pragma, appendHorizonServed).
const horizonCursorPragma = "horizon-cursor"

// rekeptConfig is the config row holding the origin's reclassification
// position: the highest rid a phantom held a check-in's P card for until it
// was filled. Any cursor below it was handed out before that arrival.
const rekeptConfig = "horizon-rekept"

// parseCursor reads a horizon-cursor pragma's one value. Anything else is no
// cursor, as zit reads a malformed number as none (sync.zig, the
// horizon_cursor_pragma arm of its request scan).
func parseCursor(c *xfer.PragmaCard) (int64, bool) {
	if len(c.Values) != 1 {
		return 0, false
	}
	n, err := strconv.ParseInt(c.Values[0], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// cursorCard is the reply's or request's spelling of cursor n.
func cursorCard(n int64) *xfer.PragmaCard {
	if n < 0 {
		panic("sync.cursorCard: negative cursor")
	}
	return &xfer.PragmaCard{Name: horizonCursorPragma, Values: []string{strconv.FormatInt(n, 10)}}
}

// originPosition is the origin's position in its own artifact sequence, the
// clone SEQNO sense of rid order: the highest rid below every phantom. A
// phantom filled later keeps its old rid, so a cursor that covered it would
// hide the arrival; stopping short of the lowest phantom keeps every fill
// above every cursor handed out while it was absent. zit's store only
// appends, and it hands out its rid count (cmd/sync.zig, round_server).
func originPosition(q db.Querier) (int64, error) {
	var top, phantom sql.NullInt64
	if err := q.QueryRow("SELECT max(rid) FROM blob").Scan(&top); err != nil {
		return 0, fmt.Errorf("sync: origin position: %w", err)
	}
	if err := q.QueryRow("SELECT min(rid) FROM phantom").Scan(&phantom); err != nil {
		return 0, fmt.Errorf("sync: origin position: %w", err)
	}
	n := top.Int64
	if phantom.Valid && phantom.Int64-1 < n {
		n = phantom.Int64 - 1
	}
	return max(n, 0), nil
}

// rekeptAt is the origin's reclassification position; 0 when none.
func rekeptAt(q db.Querier) (int64, error) {
	var value string
	err := q.QueryRow("SELECT value FROM config WHERE name=?", rekeptConfig).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("sync: %s: %w", rekeptConfig, err)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, nil // an unreadable position voids nothing it cannot name
	}
	return n, nil
}

// noteRekept records the reclassification of §6.2.6 when filling phantom
// rid can bring older check-ins into a horizon's held set: a check-in
// stored by an earlier receipt names rid as its parent. One stored by this
// receipt arrived with it, so no cursor was handed out in between.
func noteRekept(q db.Querier, rid libfossil.FslID) error {
	if rid <= 0 {
		panic("sync.noteRekept: rid must be positive")
	}
	var one int
	err := q.QueryRow(`SELECT 1 FROM plink p JOIN blob c ON c.rid=p.cid
		WHERE p.pid=? AND c.rcvid IS NOT (SELECT rcvid FROM blob WHERE rid=?) LIMIT 1`,
		rid, rid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sync: reclassification of rid=%d: %w", rid, err)
	}
	at, err := rekeptAt(q)
	if err != nil || int64(rid) <= at {
		return err
	}
	if _, err := q.Exec("REPLACE INTO config(name, value, mtime) VALUES(?, ?, strftime('%s','now'))",
		rekeptConfig, strconv.FormatInt(int64(rid), 10)); err != nil {
		return fmt.Errorf("sync: %s: %w", rekeptConfig, err)
	}
	return nil
}

// cursorRecord is what a horizon store keeps of one origin (§6.2.6): the
// cursor the origin's last reply handed out, and the position in this
// store's own rid sequence through which the origin is known to hold it
// (zit's SyncCursor, cmd/sync.zig: peer and local).
type cursorRecord struct {
	peer  int64 // the origin's cursor; 0 for none
	local int64 // this store's rids the origin holds through; 0 for none
	found bool
}

// cursorConfigName is the config row of origin's record. One row per
// origin; a record names the cone it was kept under, since a cursor from
// another cone hides artifacts this one newly keeps (zit's readSyncCursor).
func cursorConfigName(origin string) string {
	if origin == "" {
		panic("sync.cursorConfigName: origin must not be empty")
	}
	return "horizon-cursor:" + strings.TrimRight(origin, "/")
}

// loadCursor reads origin's record under the cone whose digest is digest.
// A record that does not parse, or was kept under another cone, is none.
func loadCursor(q db.Querier, origin, digest string) (cursorRecord, error) {
	var value string
	err := q.QueryRow("SELECT value FROM config WHERE name=?", cursorConfigName(origin)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return cursorRecord{}, nil
	}
	if err != nil {
		return cursorRecord{}, fmt.Errorf("sync: horizon cursor: %w", err)
	}
	fields := strings.Fields(value)
	if len(fields) != 3 || fields[0] != digest {
		return cursorRecord{}, nil
	}
	peer, perr := strconv.ParseInt(fields[1], 10, 64)
	local, lerr := strconv.ParseInt(fields[2], 10, 64)
	if perr != nil || lerr != nil || peer < 0 || local < 0 {
		return cursorRecord{}, nil
	}
	return cursorRecord{peer: peer, local: local, found: true}, nil
}

// storeCursor writes origin's record under digest.
func storeCursor(q db.Querier, origin, digest string, rec cursorRecord) error {
	if rec.peer < 0 || rec.local < 0 {
		panic("sync.storeCursor: negative position")
	}
	value := fmt.Sprintf("%s %d %d", digest, rec.peer, rec.local)
	if _, err := q.Exec("REPLACE INTO config(name, value, mtime) VALUES(?, ?, strftime('%s','now'))",
		cursorConfigName(origin), value); err != nil {
		return fmt.Errorf("sync: horizon cursor: %w", err)
	}
	return nil
}

// storePosition is the highest rid this store holds a row for: its own
// artifact count, in the sense zit's logBackedCount is.
func storePosition(q db.Querier) (int64, error) {
	var n int64
	if err := q.QueryRow("SELECT coalesce(max(rid),0) FROM blob").Scan(&n); err != nil {
		return 0, fmt.Errorf("sync: store position: %w", err)
	}
	return n, nil
}
