//go:build !js

package conformance

import (
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/checkout"
	"github.com/danmestas/go-libfossil/simio"
)

func TestSparseWorktreeManagedRecordOnly(t *testing.T) {
	if err := scManagedRecordOnly(t); err != nil {
		t.Fatal(err)
	}
}

func scManagedRecordOnly(t *testing.T) error {
	for _, tree := range []bool{false, true} {
		f := newSCRepo(t, tree)
		co := f.open(t, scDefault(t), f.c1)
		marker, err := checkout.FindCheckoutDB(simio.OSStorage{}, co.Dir(), false)
		if err != nil {
			return err
		}
		ckdb, err := db.OpenSQL(marker, db.OpenConfig{}, nil)
		if err != nil {
			return err
		}
		var count int
		err = ckdb.QueryRow("SELECT count(*) FROM vfile").Scan(&count)
		closeErr := ckdb.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if count != len(scDefaultCovered) {
			return fmt.Errorf("tree=%v: managed record has %d rows, want %d covered files", tree, count, len(scDefaultCovered))
		}
	}
	return nil
}
