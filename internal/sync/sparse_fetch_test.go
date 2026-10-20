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
	t.Run("failed-fetch-preserves-policy-intent", func(t *testing.T) {
		var beforeRID int64
		if err := r.DB().QueryRow("SELECT rid FROM blob WHERE uuid=?", outside).Scan(&beforeRID); err != nil {
			t.Fatal(err)
		}
		failure := errors.New("configured peer unavailable")
		failed := &countingTransport{failures: 1, fail: failure}
		if _, err := FetchCone(context.Background(), r, failed, nil, SyncOpts{}); !errors.Is(err, failure) {
			t.Fatalf("failed policy fetch: %v", err)
		}
		if failed.attempts != 1 {
			t.Fatalf("permanent failure attempts=%d, want 1", failed.attempts)
		}
		if got, err := content.StoreCone(r.DB()); err != nil || got != nil {
			t.Fatalf("failed fetch discarded widened intent: cone=%v, err=%v", got, err)
		}
		var afterRID, size, phantomCount int64
		if err := r.DB().QueryRow("SELECT rid,size FROM blob WHERE uuid=?", outside).Scan(&afterRID, &size); err != nil {
			t.Fatal(err)
		}
		if err := r.DB().QueryRow("SELECT count(*) FROM phantom WHERE rid=?", afterRID).Scan(&phantomCount); err != nil {
			t.Fatal(err)
		}
		if afterRID != beforeRID || size != -1 || phantomCount != 1 {
			t.Fatalf("failed fetch changed recoverable identity: rid=%d/%d size=%d phantoms=%d", beforeRID, afterRID, size, phantomCount)
		}
		a, err := content.LoadAbsencesContext(context.Background(), r.DB())
		if err != nil || a.Count(content.Transit) != 1 || a.Count(content.Elided) != 0 {
			t.Fatalf("failed widened fetch did not retain a transit phantom: absences=%v, err=%v", a, err)
		}
	})
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
