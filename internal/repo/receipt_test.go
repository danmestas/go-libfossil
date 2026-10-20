package repo

import (
	"path/filepath"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/simio"
)

// A Repo given a receipt records under it whatever its transactions store,
// bound ones included; the Repo it came from keeps recording on its own.
func TestWithReceiptCarriesIntoTransactions(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), "alice", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()
	rc := &db.Receipt{UID: 1}
	rr := r.WithReceipt(rc)
	var inTx, inBound, inBoundTx, plain int64
	if err := rr.WithTx(func(tx *db.Tx) error {
		inTx, err = db.ReceiptID(tx)
		return err
	}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if err := rr.WithBoundTx(func(b *Repo) error {
		if inBound, err = db.ReceiptID(b.DB()); err != nil {
			return err
		}
		return b.WithTx(func(tx *db.Tx) error {
			inBoundTx, err = db.ReceiptID(tx)
			return err
		})
	}); err != nil {
		t.Fatalf("WithBoundTx: %v", err)
	}
	if err := r.WithTx(func(tx *db.Tx) error {
		plain, err = db.ReceiptID(tx)
		return err
	}); err != nil {
		t.Fatalf("plain WithTx: %v", err)
	}
	want, err := rc.ID(r.DB())
	if err != nil {
		t.Fatalf("rc.ID: %v", err)
	}
	if inTx != want || inBound != want || inBoundTx != want {
		t.Fatalf("receipts = %d, %d, %d; want %d each", inTx, inBound, inBoundTx, want)
	}
	if plain == want {
		t.Fatalf("a Repo without the receipt recorded under it")
	}
}

// LocalUID names the local user content is credited to: the login when the
// repository has it, otherwise its setup user, as fossil's clone falls back
// to the user holding s.
func TestLocalUID(t *testing.T) {
	r, err := Create(filepath.Join(t.TempDir(), "r.fossil"), "alice", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()
	var nobody int64
	if err := r.DB().QueryRow("SELECT uid FROM user WHERE login='nobody'").Scan(&nobody); err != nil {
		t.Fatalf("nobody uid: %v", err)
	}
	for _, tc := range []struct {
		login string
		want  int64
	}{{"alice", 1}, {"nobody", nobody}, {"stranger", 1}, {"", 1}} {
		got, err := r.LocalUID(tc.login)
		if err != nil {
			t.Fatalf("LocalUID(%q): %v", tc.login, err)
		}
		if got != tc.want {
			t.Errorf("LocalUID(%q) = %d, want %d", tc.login, got, tc.want)
		}
	}
}
