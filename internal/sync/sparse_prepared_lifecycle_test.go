package sync

import (
	"context"
	"strings"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestPreparedSparseServiceSameRequestMutation(t *testing.T) {
	r := setupSyncTestRepo(t)
	if _, err := r.DB().Exec("UPDATE user SET cap='goix' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	service, err := prepareSparseService(context.Background(), r, HandleOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	data := []byte("new artifact within the same request")
	uuid := hash.SHA1(data)
	cards := preparedCloneCards()[:3]
	cards = append(cards, &xfer.PushCard{ServerCode: "test", ProjectCode: testPC},
		&xfer.FileCard{UUID: uuid, Content: data},
		&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true})
	resp, applied, err := service.handle(context.Background(), &xfer.Message{Cards: cards})
	if err != nil || applied {
		t.Fatalf("mutating response: applied=%v err=%v", applied, err)
	}
	if !preparedHasFile(resp, uuid) {
		for _, card := range resp.Cards {
			t.Logf("reply %T: %+v", card, card)
		}
		t.Fatal("same-request newly stored artifact missing from dense clone")
	}
	if _, exists := blob.Exists(r.DB(), uuid); !exists {
		t.Fatal("fallback did not commit accepted file")
	}
}

func TestPreparedSparseServiceFallbackPreservesOriginRefusal(t *testing.T) {
	r := setupSyncTestRepo(t)
	c, err := cone.New([]string{"src"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.SetStoreCone(r.DB(), c); err != nil {
		t.Fatal(err)
	}
	service, err := prepareSparseService(context.Background(), r, HandleOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	storeTestBlob(t, r, []byte("invalidate prepared snapshot"))
	cards := preparedCloneCards()
	cards[2] = &xfer.PragmaCard{Name: "cone", Values: []string{"zzz"}}
	resp, applied, err := service.handle(context.Background(), &xfer.Message{Cards: cards})
	if err != nil || applied {
		t.Fatalf("fallback: applied=%v err=%v", applied, err)
	}
	for _, card := range resp.Cards {
		if c, ok := card.(*xfer.ErrorCard); ok && strings.Contains(c.Message, "Sparse cone does not cover requested clone") {
			return
		}
	}
	t.Fatal("dense fallback removed the request cone and bypassed origin coverage")
}

func TestPreparedSparseServiceAdvertisedClusterSurvivesRestart(t *testing.T) {
	r := setupSyncTestRepo(t)
	member := storeTestBlob(t, r, []byte("durable prepared cluster member"))
	cluster := &deck.Deck{M: []string{member}}
	data, err := cluster.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var uuid string
	if err := r.WithTx(func(tx *db.Tx) error {
		rid, name, err := blob.Store(tx, data)
		if err != nil {
			return err
		}
		uuid = name
		return manifest.CrosslinkCluster(tx, rid, cluster)
	}); err != nil {
		t.Fatal(err)
	}
	service, err := prepareSparseService(context.Background(), r, HandleOpts{})
	if err != nil {
		t.Fatal(err)
	}
	transport := &preparedSparseTransport{service: service}
	cards := preparedCloneCards()[:3]
	cards = append(cards, &xfer.PullCard{ServerCode: "test", ProjectCode: testPC})
	resp, err := transport.Exchange(context.Background(), &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	announced := false
	for _, card := range resp.Cards {
		if c, ok := card.(*xfer.IGotCard); ok && c.UUID == uuid {
			announced = true
		}
	}
	if !announced || transport.applied != 1 {
		t.Fatalf("prepared pull announced=%v prepared=%d", announced, transport.applied)
	}
	storeTestBlob(t, r, []byte("generation after advertisement"))
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	path := r.Path()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := repo.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service, err = prepareSparseService(context.Background(), reopened, HandleOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	transport = &preparedSparseTransport{service: service}
	cards = append(cards, &xfer.GimmeCard{UUID: uuid})
	resp, err = transport.Exchange(context.Background(), &xfer.Message{Cards: cards})
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range resp.Cards {
		if c, ok := card.(*xfer.FileCard); ok && c.UUID == uuid {
			if string(c.Content) != string(data) || hash.SHA3(c.Content) != uuid {
				t.Fatal("advertised cluster changed across refresh/restart")
			}
			return
		}
	}
	t.Fatal("advertised cluster unavailable after refresh/restart")
}
