package sync

import (
	"context"
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestCloneExcludedCandidatesExhaustWithoutEmptyPage(t *testing.T) {
	for _, count := range []int{17, 2051} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r := setupSyncTestRepo(t)
			for i := 0; i < count; i++ {
				uuid := storeAndLink(t, r, []byte(fmt.Sprintf("private candidate %d", i)))
				if _, err := r.DB().Exec("INSERT INTO private(rid) SELECT rid FROM blob WHERE uuid = ?", uuid); err != nil {
					t.Fatal(err)
				}
			}
			resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
				&xfer.PragmaCard{Name: "cone", Values: []string{"src"}},
				&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true},
			}})
			if err != nil {
				t.Fatal(err)
			}
			var next int
			for _, card := range resp.Cards {
				switch c := card.(type) {
				case *xfer.CloneSeqNoCard:
					next = c.SeqNo
				case *xfer.FileCard, *xfer.CFileCard:
					t.Fatal("private candidate emitted")
				}
			}
			// An intermediate empty positive page would stop a real clone
			// under the pinned sync protocol's Section 3.7.
			if next != 0 {
				t.Fatalf("next RID = %d, want exhaustion", next)
			}
		})
	}
}
