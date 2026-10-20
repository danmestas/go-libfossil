package sync

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestFetchConeExplicitPeer(t *testing.T) {
	source := setupSyncTestRepo(t)
	inside := storeAndLink(t, source, []byte("inside"))
	outside := storeAndLink(t, source, []byte("outside"))
	data, err := (&deck.Deck{C: "policy fetch", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "src/a", UUID: inside}, {Name: "zzz/a", UUID: outside}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	storeAndLink(t, source, data)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	peer := &MockTransport{Handler: func(req *xfer.Message) *xfer.Message {
		calls++
		resp, err := HandleSync(context.Background(), source, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}}
	r, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "fetch.fossil"), peer, CloneOpts{StoreCone: c, PullPhantoms: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, ok := content.AvailableByUUID(r.DB(), outside); ok {
		t.Fatal("outside content unexpectedly held")
	}
	before := calls
	_, err = FetchCone(context.Background(), r, nil, nil, SyncOpts{})
	if err == nil || !strings.Contains(err.Error(), "1 elided") || !strings.Contains(err.Error(), "no configured peer") || strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("no-peer diagnostic: %v", err)
	}
	if calls != before {
		t.Fatal("no-peer fetch used a network")
	}
	if got, _ := content.StoreCone(r.DB()); got == nil || !got.Equal(c) {
		t.Fatal("no-peer refusal changed policy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchCone(ctx, r, peer, nil, SyncOpts{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled demand: %v", err)
	}
	if calls != before {
		t.Fatal("cancelled demand used peer")
	}
	res, err := FetchCone(context.Background(), r, peer, nil, SyncOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.FilesRecvd == 0 {
		t.Fatalf("no fetch progress: %+v", res)
	}
	if _, ok := content.AvailableByUUID(r.DB(), outside); !ok {
		t.Fatal("widened file not fetched")
	}
	if got, err := content.StoreCone(r.DB()); err != nil || got != nil {
		t.Fatalf("dense policy: %v %v", got, err)
	}
}
