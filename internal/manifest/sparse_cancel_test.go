package manifest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
)

func TestSparseMetadataCancellationRollsBackBatch(t *testing.T) {
	f, rid, _, _ := partialMetadataFixture(t)
	for i := 0; i < 768; i++ {
		if _, _, err := blob.Store(f.r.DB(), fmt.Appendf(nil, "ordinary payload %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := newPollCancelCtx(2)
	linked, err := CrosslinkContext(ctx, f.r)
	if !errors.Is(err, context.Canceled) || linked != 0 {
		t.Fatalf("cancelled sparse sweep = %d, %v", linked, err)
	}
	for _, query := range []string{"SELECT count(*) FROM event WHERE objid=?", "SELECT count(*) FROM orphan WHERE rid=?", "SELECT count(*) FROM tagxref WHERE rid=?"} {
		var n int
		if err := f.r.DB().QueryRow(query, rid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("cancelled batch leaked rows: %s = %d", query, n)
		}
	}
	if _, err := Crosslink(f.r); err != nil {
		t.Fatal(err)
	}
	assertSparseRows(t, f, int64(rid), 1, 0, 1)
}
