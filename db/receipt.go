package db

import (
	"database/sql"
	"errors"
	"fmt"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/simio"
)

// Receipt is one arrival of content in the repository: a commit, a sync or
// clone, a push a server accepts. Fossil records each as a row of rcvfrom,
// made the first time that arrival stores anything (content_rcvid_init in
// src/content.c), and every blob or unversioned file it stores names the row
// in its rcvid. A receipt that stores nothing leaves no row.
//
// The fields are what the row will hold, read when it is made: UID is the
// user.uid the content came from, Nonce the nonce of the login card that
// authenticated it, and IPAddr the peer's address or, for local content, a
// description of its source. An empty Nonce or IPAddr is stored as NULL, as
// fossil stores an absent one. The row's mtime is the Julian day of the
// moment it is made, by Clock (nil means real time).
//
// A Receipt is not safe for concurrent use.
type Receipt struct {
	UID    int64
	Nonce  string
	IPAddr string
	Clock  simio.Clock

	rcvid int64   // the row made for this receipt, 0 before one is
	mtime float64 // that row's mtime, which tells it from a reused rcvid
}

// ID returns the rcvid of the receipt's row, making the row through q if the
// receipt has none yet. A row made in a transaction that rolled back is gone,
// so the receipt checks its row is still there and makes another if not.
func (rc *Receipt) ID(q Querier) (int64, error) {
	if rc == nil {
		panic("db.Receipt.ID: receiver must not be nil")
	}
	if q == nil {
		panic("db.Receipt.ID: q must not be nil")
	}
	if rc.rcvid != 0 {
		var one int
		err := q.QueryRow("SELECT 1 FROM rcvfrom WHERE rcvid=? AND mtime=?",
			rc.rcvid, rc.mtime).Scan(&one)
		if err == nil {
			return rc.rcvid, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("db.Receipt: check rcvfrom %d: %w", rc.rcvid, err)
		}
	}
	clock := rc.Clock
	if clock == nil {
		clock = simio.RealClock{}
	}
	mtime := libfossil.JulianDayFromMillis(clock.Now().UnixMilli())
	res, err := q.Exec("INSERT INTO rcvfrom(uid, mtime, nonce, ipaddr) VALUES(?, ?, ?, ?)",
		rc.UID, mtime, nullIfEmpty(rc.Nonce), nullIfEmpty(rc.IPAddr))
	if err != nil {
		return 0, fmt.Errorf("db.Receipt: insert rcvfrom: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db.Receipt: rcvfrom id: %w", err)
	}
	if id <= 0 {
		panic("db.Receipt.ID: rcvfrom insert returned a non-positive rcvid")
	}
	rc.rcvid, rc.mtime = id, mtime
	return id, nil
}

// nullIfEmpty binds "" as NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// UseReceipt makes rc the receipt content stored in t is recorded under, and
// returns the one it replaces so a caller can put it back.
func (t *Tx) UseReceipt(rc *Receipt) *Receipt {
	if t == nil {
		panic("db.Tx.UseReceipt: receiver must not be nil")
	}
	prev := t.receipt
	t.receipt = rc
	return prev
}

// UseReceipt is Tx.UseReceipt for the transaction a bound DB runs in. Only a
// bound DB has a transaction to give it to.
func (d *DB) UseReceipt(rc *Receipt) *Receipt {
	if d == nil {
		panic("db.DB.UseReceipt: receiver must not be nil")
	}
	tx := d.boundTx()
	if tx == nil {
		panic("db.DB.UseReceipt: only a DB bound to a transaction carries a receipt")
	}
	return tx.UseReceipt(rc)
}

// ReceiptID returns the rcvid content stored through q is recorded under: the
// receipt of q's transaction, which is the one given by UseReceipt or, when
// none was, one made for that transaction alone. Content stored outside a
// transaction is a receipt of its own.
func ReceiptID(q Querier) (int64, error) {
	if q == nil {
		panic("db.ReceiptID: q must not be nil")
	}
	tx, _ := q.(*Tx)
	if d, ok := q.(*DB); ok {
		tx = d.boundTx()
	}
	if tx == nil {
		return (&Receipt{}).ID(q)
	}
	if tx.receipt == nil {
		tx.receipt = &Receipt{}
	}
	return tx.receipt.ID(tx)
}
