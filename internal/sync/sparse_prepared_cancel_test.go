package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// A canceled caller must not enter a bound transaction or fall back to dense
// inventory work, even when observer validation would also report staleness.
func TestPreparedSparseServiceCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			r := setupSyncTestRepo(t)
			storeTestBlob(t, r, []byte("must not be served after cancellation"))
			service, err := prepareSparseService(context.Background(), r, HandleOpts{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
				want = context.DeadlineExceeded
			}
			cancel()
			resp, applied, err := service.handle(ctx, &xfer.Message{Cards: preparedCloneCards()})
			if !errors.Is(err, want) || resp != nil || applied {
				t.Fatalf("cancellation became dense response: applied=%v resp=%v err=%v want=%v",
					applied, resp, err, want)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			_, _, err = service.handle(ctx, &xfer.Message{Cards: preparedCloneCards()})
			if !errors.Is(err, want) {
				t.Fatalf("canceled call touched closed repository: %v", err)
			}
		})
	}
}
