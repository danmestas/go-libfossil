package db_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/simio"
)

func newSchemaDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "r.fossil"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.CreateRepoSchema(d); err != nil {
		t.Fatalf("CreateRepoSchema: %v", err)
	}
	return d
}

func rcvfromCount(t *testing.T, q db.Querier) int {
	t.Helper()
	var n int
	if err := q.QueryRow("SELECT count(*) FROM rcvfrom").Scan(&n); err != nil {
		t.Fatalf("count rcvfrom: %v", err)
	}
	return n
}

// A new repository has received nothing, so it has no rcvfrom row: fossil
// adds one only when content arrives (content_rcvid_init).
func TestNewSchemaHasNoReceipt(t *testing.T) {
	d := newSchemaDB(t)
	if n := rcvfromCount(t, d); n != 0 {
		t.Fatalf("rcvfrom rows = %d, want 0", n)
	}
}

// A receipt writes its row the first time it is asked for its id, with
// fossil's column values: mtime the Julian day of the receipt, and an empty
// nonce or address stored as NULL. Asking again reuses the row.
func TestReceiptRecordsOneRowWhenFirstUsed(t *testing.T) {
	d := newSchemaDB(t)
	at := time.Date(2026, 10, 18, 12, 34, 56, 789_000_000, time.UTC)
	rc := &db.Receipt{UID: 7, Nonce: "abc", IPAddr: "10.0.0.1", Clock: simio.NewSimClockAt(at)}
	if n := rcvfromCount(t, d); n != 0 {
		t.Fatalf("rcvfrom rows before use = %d, want 0", n)
	}
	first, err := rc.ID(d)
	if err != nil {
		t.Fatalf("ID: %v", err)
	}
	again, err := rc.ID(d)
	if err != nil {
		t.Fatalf("ID again: %v", err)
	}
	if first <= 0 || again != first {
		t.Fatalf("ids = %d, %d; want one positive id", first, again)
	}
	if n := rcvfromCount(t, d); n != 1 {
		t.Fatalf("rcvfrom rows = %d, want 1", n)
	}
	var uid int64
	var mtime float64
	var nonce, ip string
	// mtime+0 reads the stored REAL as a number: the column is declared
	// DATETIME, which some drivers would otherwise scan as a time.
	if err := d.QueryRow("SELECT uid, mtime+0, nonce, ipaddr FROM rcvfrom"+
		" WHERE rcvid=? AND typeof(mtime)='real'", first).Scan(&uid, &mtime, &nonce, &ip); err != nil {
		t.Fatalf("read row: %v", err)
	}
	want := libfossil.JulianDayFromMillis(at.UnixMilli())
	if uid != 7 || mtime != want || nonce != "abc" || ip != "10.0.0.1" {
		t.Fatalf("row = (%d, %v, %q, %q), want (7, %v, \"abc\", \"10.0.0.1\")",
			uid, mtime, nonce, ip, want)
	}

	local := &db.Receipt{UID: 1}
	id, err := local.ID(d)
	if err != nil {
		t.Fatalf("local ID: %v", err)
	}
	var nulls int
	if err := d.QueryRow("SELECT count(*) FROM rcvfrom WHERE rcvid=? AND nonce IS NULL"+
		" AND ipaddr IS NULL AND typeof(mtime)='real'", id).Scan(&nulls); err != nil {
		t.Fatalf("read local row: %v", err)
	}
	if nulls != 1 {
		t.Fatalf("local receipt row does not have NULL nonce and ipaddr and a real mtime")
	}
}

// A receipt whose row was rolled back records a new one on its next use, so
// no blob is ever left naming a row that does not exist.
func TestReceiptRecordsAgainAfterRollback(t *testing.T) {
	d := newSchemaDB(t)
	rc := &db.Receipt{UID: 1}
	errUndo := errors.New("undo")
	err := d.WithTx(func(tx *db.Tx) error {
		if _, err := rc.ID(tx); err != nil {
			return err
		}
		return errUndo
	})
	if !errors.Is(err, errUndo) {
		t.Fatalf("WithTx = %v, want errUndo", err)
	}
	id, err := rc.ID(d)
	if err != nil {
		t.Fatalf("ID after rollback: %v", err)
	}
	var n int
	if err := d.QueryRow("SELECT count(*) FROM rcvfrom WHERE rcvid=?", id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("receipt %d has no row after its first was rolled back", id)
	}
}

// A transaction stores under the receipt it was given; one given none gets a
// receipt of its own, shared by everything stored in it.
func TestReceiptIDFollowsTransaction(t *testing.T) {
	d := newSchemaDB(t)
	rc := &db.Receipt{UID: 3}
	err := d.WithTx(func(tx *db.Tx) error {
		a, err := db.ReceiptID(tx)
		if err != nil {
			return err
		}
		b, err := db.ReceiptID(tx)
		if err != nil {
			return err
		}
		if a != b {
			t.Errorf("a transaction without a receipt used two: %d, %d", a, b)
		}
		prev := tx.UseReceipt(rc)
		got, err := db.ReceiptID(tx)
		if err != nil {
			return err
		}
		want, err := rc.ID(tx)
		if err != nil {
			return err
		}
		if got != want || got == a {
			t.Errorf("ReceiptID = %d, want the given receipt's %d", got, want)
		}
		tx.UseReceipt(prev)
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if n := rcvfromCount(t, d); n != 2 {
		t.Fatalf("rcvfrom rows = %d, want 2", n)
	}
}

// A DB bound to a transaction stores under that transaction's receipt.
func TestReceiptIDOnBoundDB(t *testing.T) {
	d := newSchemaDB(t)
	rc := &db.Receipt{UID: 5}
	err := d.WithBoundTx(func(b *db.DB) error {
		prev := b.UseReceipt(rc)
		defer b.UseReceipt(prev)
		got, err := db.ReceiptID(b)
		if err != nil {
			return err
		}
		want, err := rc.ID(b)
		if err != nil {
			return err
		}
		if got != want {
			t.Errorf("ReceiptID(bound) = %d, want %d", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithBoundTx: %v", err)
	}
}

// receiptOwner reports the uid of the rcvfrom row id, which tells whose
// receipt it is.
func receiptOwner(t *testing.T, q db.Querier, id int64) int64 {
	t.Helper()
	var uid int64
	if err := q.QueryRow("SELECT uid FROM rcvfrom WHERE rcvid=?", id).Scan(&uid); err != nil {
		t.Fatalf("rcvfrom %d: %v", id, err)
	}
	return uid
}

// A receipt whose row was rolled back must not take over the row another
// receipt makes under the same rcvid, even when a frozen clock gives both
// rows the same mtime.
func TestReceiptForgetsRolledBackRowUnderFrozenClock(t *testing.T) {
	d := newSchemaDB(t)
	clock := simio.NewSimClockAt(time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC))
	lost := &db.Receipt{UID: 1, Clock: clock}
	errUndo := errors.New("undo")
	if err := d.WithTx(func(tx *db.Tx) error {
		if _, err := lost.ID(tx); err != nil {
			return err
		}
		return errUndo
	}); !errors.Is(err, errUndo) {
		t.Fatalf("WithTx = %v, want errUndo", err)
	}
	other := &db.Receipt{UID: 2, Clock: clock}
	otherID, err := other.ID(d)
	if err != nil {
		t.Fatalf("other.ID: %v", err)
	}
	id, err := lost.ID(d)
	if err != nil {
		t.Fatalf("lost.ID: %v", err)
	}
	if id == otherID {
		t.Fatalf("the rolled-back receipt took over rcvid %d, another receipt's row", id)
	}
	if uid := receiptOwner(t, d, id); uid != 1 {
		t.Fatalf("receipt row %d credits uid %d, want 1", id, uid)
	}
}

// The same holds for a row undone with the savepoint that made it, inside a
// transaction that goes on and commits; a row a released savepoint made
// stays the receipt's.
func TestReceiptForgetsRowOfRolledBackSavepoint(t *testing.T) {
	d := newSchemaDB(t)
	clock := simio.NewSimClockAt(time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC))
	lost := &db.Receipt{UID: 1, Clock: clock}
	kept := &db.Receipt{UID: 3, Clock: clock}
	other := &db.Receipt{UID: 2, Clock: clock}
	errUndo := errors.New("undo")
	var keptID, otherID, lostID int64
	err := d.WithBoundTx(func(b *db.DB) error {
		if err := b.WithTx(func(tx *db.Tx) error {
			var err error
			keptID, err = kept.ID(tx)
			return err
		}); err != nil {
			return err
		}
		if err := b.WithTx(func(tx *db.Tx) error {
			if _, err := lost.ID(tx); err != nil {
				return err
			}
			return errUndo
		}); !errors.Is(err, errUndo) {
			t.Fatalf("savepoint = %v, want errUndo", err)
		}
		var err error
		if otherID, err = other.ID(b); err != nil {
			return err
		}
		if lostID, err = lost.ID(b); err != nil {
			return err
		}
		again, err := kept.ID(b)
		if err != nil {
			return err
		}
		if again != keptID {
			t.Errorf("a released savepoint's receipt moved from %d to %d", keptID, again)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithBoundTx: %v", err)
	}
	if lostID == otherID {
		t.Fatalf("the rolled-back receipt took over rcvid %d, another receipt's row", lostID)
	}
	if uid := receiptOwner(t, d, lostID); uid != 1 {
		t.Fatalf("receipt row %d credits uid %d, want 1", lostID, uid)
	}
	if uid := receiptOwner(t, d, keptID); uid != 3 {
		t.Fatalf("receipt row %d credits uid %d, want 3", keptID, uid)
	}
}

// Once made and kept, a receipt's id costs no query: storing many blobs under
// one receipt does not read rcvfrom for each.
func TestReceiptIDReusesWithoutQuery(t *testing.T) {
	d := newSchemaDB(t)
	rc := &db.Receipt{UID: 1}
	if _, err := rc.ID(d); err != nil {
		t.Fatalf("ID: %v", err)
	}
	if _, err := d.Exec("DROP TABLE rcvfrom"); err != nil {
		t.Fatalf("drop rcvfrom: %v", err)
	}
	if _, err := rc.ID(d); err != nil {
		t.Fatalf("a kept receipt's id read rcvfrom: %v", err)
	}
}
