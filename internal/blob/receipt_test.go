package blob

import (
	"testing"

	"github.com/danmestas/go-libfossil/db"
)

func blobRcvid(t *testing.T, q db.Querier, uuid string) int64 {
	t.Helper()
	var rcvid int64
	if err := q.QueryRow("SELECT rcvid FROM blob WHERE uuid=?", uuid).Scan(&rcvid); err != nil {
		t.Fatalf("read rcvid of %s: %v", uuid, err)
	}
	return rcvid
}

// Every blob stored in a transaction names that transaction's receipt, as
// fossil's content_put_ex names g.rcvid; a phantom names none (content_new
// stores rcvid 0), and filling it names the receipt that brought its content.
func TestStoreRecordsTheReceipt(t *testing.T) {
	d := setupTestDB(t)
	// An earlier receipt holds rcvid 1, so a store that names 1 regardless
	// cannot pass for one that names its own receipt.
	if _, err := (&db.Receipt{UID: 9}).ID(d); err != nil {
		t.Fatalf("earlier receipt: %v", err)
	}
	rc := &db.Receipt{UID: 1}
	var full, delta, phantom string
	err := d.WithTx(func(tx *db.Tx) error {
		tx.UseReceipt(rc)
		_, u, err := Store(tx, []byte("first blob content, long enough to delta against\n"))
		if err != nil {
			return err
		}
		full = u
		src, _ := Exists(tx, full)
		_, u, err = StoreDelta(tx, []byte("first blob content, long enough to delta against!\n"), src)
		if err != nil {
			return err
		}
		delta = u
		phantom = "1111111111111111111111111111111111111111"
		_, err = StorePhantom(tx, phantom)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	want, err := rc.ID(d)
	if err != nil {
		t.Fatalf("receipt id: %v", err)
	}
	if got := blobRcvid(t, d, full); got != want {
		t.Errorf("full blob rcvid = %d, want %d", got, want)
	}
	if got := blobRcvid(t, d, delta); got != want {
		t.Errorf("delta blob rcvid = %d, want %d", got, want)
	}
	if got := blobRcvid(t, d, phantom); got != 0 {
		t.Errorf("phantom rcvid = %d, want 0", got)
	}
	var rows int
	if err := d.QueryRow("SELECT count(*) FROM rcvfrom").Scan(&rows); err != nil {
		t.Fatalf("count rcvfrom: %v", err)
	}
	if rows != 2 {
		t.Errorf("rcvfrom rows = %d, want 2", rows)
	}

	// Storing content already held records nothing new.
	later := &db.Receipt{UID: 1}
	if err := d.WithTx(func(tx *db.Tx) error {
		tx.UseReceipt(later)
		_, _, err := Store(tx, []byte("first blob content, long enough to delta against\n"))
		return err
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := d.QueryRow("SELECT count(*) FROM rcvfrom").Scan(&rows); err != nil {
		t.Fatalf("count rcvfrom: %v", err)
	}
	if rows != 2 {
		t.Errorf("rcvfrom rows after storing held content = %d, want 2", rows)
	}
}

// Every stored blob names an rcvfrom row that exists, even one stored with no
// receipt given.
func TestStoreWithoutReceiptNamesARow(t *testing.T) {
	d := setupTestDB(t)
	_, u, err := Store(d, []byte("loose content\n"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	var n int
	if err := d.QueryRow("SELECT count(*) FROM blob JOIN rcvfrom USING(rcvid) WHERE uuid=?",
		u).Scan(&n); err != nil {
		t.Fatalf("join rcvfrom: %v", err)
	}
	if n != 1 {
		t.Fatalf("blob %s names no rcvfrom row", u)
	}
}
