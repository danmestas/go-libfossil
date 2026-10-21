package sync

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// The fatal sequence card rolls back with a nil Go error, after gimme has
// expanded a newly pushed artifact. A later committed blob reuses its RID.
func TestPreparedSparseServiceRollbackDoesNotPoisonCache(t *testing.T) {
	r := setupSyncTestRepo(t)
	if _, err := r.DB().Exec("UPDATE user SET cap='goix' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	cache := content.NewCache(1 << 20)
	service, err := prepareSparseService(context.Background(), r, HandleOpts{ContentCache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	var nextRID libfossil.FslID
	if err := r.DB().QueryRow("SELECT coalesce(max(rid),0)+1 FROM blob").Scan(&nextRID); err != nil {
		t.Fatal(err)
	}
	a := []byte("artifact A is rolled back")
	aUUID := hash.SHA1(a)
	cards := []xfer.Card{versionPragma("client-version"),
		&xfer.PullCard{ServerCode: "test", ProjectCode: testPC},
		&xfer.PushCard{ServerCode: "test", ProjectCode: testPC},
		&xfer.FileCard{UUID: aUUID, Content: a}, &xfer.GimmeCard{UUID: aUUID},
		&xfer.CloneCard{Version: 3, SeqNo: 0, SeqNoIsDecimal: true}}
	transport := &preparedSparseTransport{service: service}
	resp, err := transport.Exchange(context.Background(), &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	fatal := false
	for _, card := range resp.Cards {
		if _, ok := card.(*xfer.ErrorCard); ok {
			fatal = true
		}
	}
	if !fatal {
		t.Fatal("fixture did not hit protocol fatal")
	}
	if _, exists := blob.Exists(r.DB(), aUUID); exists {
		t.Fatal("protocol fatal did not actually roll back A")
	}
	b := []byte("artifact B reuses the uncommitted RID")
	bRID, bUUID, err := blob.Store(r.DB(), b)
	if err != nil {
		t.Fatal(err)
	}
	if bRID != nextRID {
		t.Fatalf("RID not reused: got %d want %d", bRID, nextRID)
	}
	resp, err = transport.Exchange(context.Background(), &xfer.Message{Cards: []xfer.Card{
		versionPragma("client-version"), &xfer.PullCard{ServerCode: "test", ProjectCode: testPC},
		&xfer.GimmeCard{UUID: bUUID}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range resp.Cards {
		if c, ok := card.(*xfer.FileCard); ok && c.UUID == bUUID {
			if string(c.Content) != string(b) || hash.SHA3(c.Content) != bUUID {
				t.Fatalf("RID cache returned %q under B hash", c.Content)
			}
			return
		}
	}
	t.Fatal("B not returned")
}
