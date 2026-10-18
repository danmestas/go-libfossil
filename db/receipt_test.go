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
	if err := d.QueryRow("SELECT uid, mtime, nonce, ipaddr FROM rcvfrom WHERE rcvid=?",
		first).Scan(&uid, &mtime, &nonce, &ip); err != nil {
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
