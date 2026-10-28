package sync

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/delta"
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

// onDroppedSeed is a client store that refused a seed, and a delta built on
// it, as a peer that still holds the seed would send it.
func onDroppedSeed(t *testing.T) (*repo.Repo, *xfer.FileCard) {
	t.Helper()
	r := setupSyncTestRepo(t)
	seed := []byte("a refused seed's bytes, which this store no longer holds\n")
	if err := seedcheck.Refuse(r.DB(), hash.SHA1(seed)); err != nil {
		t.Fatal(err)
	}
	target := append([]byte("built on the seed\n"), seed...)
	d, err := delta.Create(seed, target)
	if err != nil {
		t.Fatal(err)
	}
	return r, &xfer.FileCard{UUID: hash.SHA1(target), DeltaSrc: hash.SHA1(seed), Content: d}
}

// A peer that answers every request with the same delta on a dropped seed
// does not hold the pull open: only the first makes a new phantom.
func TestRefusedBasisResentDoesNotContinue(t *testing.T) {
	r, card := onDroppedSeed(t)
	tr := &MockTransport{Handler: func(*xfer.Message) *xfer.Message {
		return &xfer.Message{Cards: []xfer.Card{card}}
	}}
	res, err := Sync(context.Background(), r, tr, SyncOpts{Pull: true, ProjectCode: testPC})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rounds > 3 {
		t.Fatalf("the resent delta held the pull open for %d rounds", res.Rounds)
	}
}

// The phantom a private delta on a dropped seed leaves is private: its name
// is never asked for in a public sync.
func TestRefusedBasisPhantomKeepsPrivacy(t *testing.T) {
	r, card := onDroppedSeed(t)
	sent := false
	tr := &MockTransport{Handler: func(*xfer.Message) *xfer.Message {
		if sent {
			return &xfer.Message{}
		}
		sent = true
		return &xfer.Message{Cards: []xfer.Card{&xfer.PrivateCard{}, card}}
	}}
	if _, err := Sync(context.Background(), r, tr, SyncOpts{Pull: true, Private: true, ProjectCode: testPC}); err != nil {
		t.Fatal(err)
	}
	rid, ok := blob.Exists(r.DB(), card.UUID)
	if !ok {
		t.Fatal("the delta on the dropped seed left no phantom")
	}
	if !content.IsPrivate(r.DB(), int64(rid)) {
		t.Fatal("the private delta's phantom is public")
	}
}
