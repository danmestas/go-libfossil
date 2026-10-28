package sync

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/seedcheck"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// refusedPhantom leaves r a phantom named for a refused seed, as fossil's
// rebuild makes of a dropped seed's row, or a name known before its seed
// arrived and was refused.
func refusedPhantom(t *testing.T) (*repo.Repo, string) {
	t.Helper()
	r := setupSyncTestRepo(t)
	uuid := hash.SHA1([]byte("a refused seed\n"))
	if _, err := blob.StorePhantom(r.DB(), uuid); err != nil {
		t.Fatal(err)
	}
	if err := seedcheck.Refuse(r.DB(), uuid); err != nil {
		t.Fatal(err)
	}
	return r, uuid
}

// A clone that asks for what it left missing never counts a refused seed's
// phantom among them: no pull asks for it, so it would stall the clone.
func TestPhantomUUIDsSkipsRefused(t *testing.T) {
	r, uuid := refusedPhantom(t)
	missing, err := phantomUUIDs(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if missing[uuid] {
		t.Fatal("a refused seed's phantom is counted missing")
	}
}

// An origin answering a push never asks the pusher for a refused seed.
func TestRequestPhantomsSkipsRefused(t *testing.T) {
	r, uuid := refusedPhantom(t)
	grantNobodyPush(t, r)
	resp, err := HandleSync(context.Background(), r, &xfer.Message{Cards: []xfer.Card{
		&xfer.PushCard{ServerCode: "s", ProjectCode: testPC}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cards {
		if g, ok := c.(*xfer.GimmeCard); ok && g.UUID == uuid {
			t.Fatal("the origin asked the pusher for a refused seed")
		}
	}
}
