package sync

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/deck"
	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func TestPreparedSparseServiceRejectsMutatingProfiles(t *testing.T) {
	cards := []xfer.Card{
		&xfer.PushCard{}, &xfer.FileCard{}, &xfer.CFileCard{}, &xfer.IGotCard{},
		&xfer.ConfigCard{}, &xfer.ReqConfigCard{}, &xfer.UVFileCard{}, &xfer.UVIGotCard{},
		&xfer.XRowCard{}, &xfer.XDeleteCard{}, &xfer.PrivateCard{},
		&xfer.PragmaCard{Name: "ci-lock"}, &xfer.PragmaCard{Name: "ci-unlock"},
		&xfer.PragmaCard{Name: "send-private"}, &xfer.PragmaCard{Name: "uv-hash"},
		&xfer.PragmaCard{Name: "future-mode"},
	}
	for _, card := range cards {
		t.Run(fmt.Sprintf("%T/%v", card, card), func(t *testing.T) {
			r := setupSyncTestRepo(t)
			service, err := prepareSparseService(context.Background(), r, HandleOpts{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			req := &xfer.Message{Cards: append(preparedCloneCards(), card)}
			if preparedSparseRead(req) {
				t.Fatal("mutating or unsupported mode accepted")
			}
			switch card.(type) {
			case *xfer.PushCard, *xfer.PragmaCard:
			default:
				return // Other zero-value cards are not valid handler inputs.
			}
			// The unsafe card follows clone: look-ahead must reject the proof
			// before the first content-producing card, not after mutation.
			_, applied, _ := service.handle(context.Background(), req)
			if applied {
				t.Fatal("prepared proof used before later unsupported card")
			}
		})
	}
}

func preparedCloneCards() []xfer.Card {
	return []xfer.Card{versionPragma("client-version"), sendSubmanifests(),
		&xfer.PragmaCard{Name: "cone", Values: []string{"src"}},
		&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true}}
}

func TestPreparedSparseServiceInvalidation(t *testing.T) {
	for _, kind := range []string{"source", "foreign-source", "private", "shun", "close", "observer-close", "observer-replace"} {
		t.Run(kind, func(t *testing.T) {
			r := setupSyncTestRepo(t)
			r.DB().SqlDB().SetMaxOpenConns(1)
			outside := storeAndLink(t, r, []byte("outside content"))
			data, err := (&deck.Deck{C: "outside", D: time.Now().UTC(), U: deck.User("u"), F: []deck.FileCard{{Name: "zzz/a", UUID: outside}}}).Marshal()
			if err != nil {
				t.Fatal(err)
			}
			storeAndLink(t, r, data)
			service, err := prepareSparseService(context.Background(), r, HandleOpts{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			cards := preparedCloneCards()
			resp, applied, err := service.handle(context.Background(), &xfer.Message{Cards: cards})
			if err != nil || !applied || preparedHasFile(resp, outside) {
				t.Fatalf("initial sparse response: applied=%v outside=%v err=%v", applied, preparedHasFile(resp, outside), err)
			}
			switch kind {
			case "source":
				storeTestBlob(t, r, []byte("new source"))
			case "foreign-source":
				foreign, openErr := repo.Open(r.Path())
				if openErr != nil {
					t.Fatal(openErr)
				}
				storeTestBlob(t, foreign, []byte("foreign source"))
				err = foreign.Close()
			case "private":
				_, err = r.DB().Exec("INSERT INTO private(rid) SELECT rid FROM blob WHERE uuid=?", outside)
			case "shun":
				_, err = r.DB().Exec("INSERT INTO shun(uuid,mtime) VALUES(?,0)", outside)
			case "close":
				err = service.Close()
			case "observer-close":
				err = service.observer.conn.Close()
			case "observer-replace":
				if err = service.observer.conn.Close(); err == nil {
					service.observer.conn, err = service.observer.pool.Conn(context.Background())
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			resp, applied, err = service.handle(context.Background(), &xfer.Message{Cards: cards})
			if err != nil || applied {
				t.Fatalf("invalidated response: applied=%v err=%v", applied, err)
			}
			// Match the existing dense clone path: it skips private rows but
			// does not currently skip shun rows. Shun still invalidates facts.
			wantOutside := kind != "private"
			if preparedHasFile(resp, outside) != wantOutside {
				t.Fatalf("dense fallback outside=%v want=%v", preparedHasFile(resp, outside), wantOutside)
			}
		})
	}
}

func preparedHasFile(resp *xfer.Message, uuid string) bool {
	if resp == nil {
		return false
	}
	for _, card := range resp.Cards {
		switch c := card.(type) {
		case *xfer.FileCard:
			if c.UUID == uuid {
				return true
			}
		case *xfer.CFileCard:
			if c.UUID == uuid {
				return true
			}
		}
	}
	return false
}

type preparedSparseTransport struct {
	service *preparedSparseService
	applied int
}

func (p *preparedSparseTransport) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	wire, err := req.Encode()
	if err != nil {
		return nil, err
	}
	req, err = xfer.Decode(wire, xfer.ContentTypeCompressed)
	if err != nil {
		return nil, err
	}
	resp, applied, err := p.service.handle(ctx, req)
	if err != nil {
		return nil, err
	}
	if applied {
		p.applied++
	}
	wire, err = resp.Encode()
	if err != nil {
		return nil, err
	}
	return xfer.Decode(wire, xfer.ContentTypeCompressed)
}

func TestPreparedSparseServiceCloneExcludedRun(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprint(private), func(t *testing.T) {
			r := setupSyncTestRepo(t)
			r.DB().SqlDB().SetMaxOpenConns(1)
			var files []deck.FileCard
			var excluded string
			for i := 0; i < 2051; i++ {
				uuid := storeAndLink(t, r, []byte(fmt.Sprintf("excluded %d", i)))
				files = append(files, deck.FileCard{Name: fmt.Sprintf("zzz/%04d", i), UUID: uuid})
				excluded = uuid
				if private {
					if _, err := r.DB().Exec("INSERT INTO private(rid) SELECT rid FROM blob WHERE uuid=?", uuid); err != nil {
						t.Fatal(err)
					}
				}
			}
			final := storeAndLink(t, r, []byte("final in-cone content"))
			files = append([]deck.FileCard{{Name: "src/final", UUID: final}}, files...)
			data, err := (&deck.Deck{C: "prepared clone", D: time.Now().UTC(), U: deck.User("u"), F: files}).Marshal()
			if err != nil {
				t.Fatal(err)
			}
			storeAndLink(t, r, data)
			service, err := prepareSparseService(context.Background(), r, HandleOpts{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			transport := &preparedSparseTransport{service: service}
			c, err := cone.New([]string{"src"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			cloned, _, err := Clone(context.Background(), filepath.Join(t.TempDir(), "clone.fossil"), transport, CloneOpts{StoreCone: c, PullPhantoms: true})
			if err != nil {
				t.Fatal(err)
			}
			defer cloned.Close()
			if transport.applied == 0 {
				t.Fatal("real client never selected prepared path")
			}
			var rid libfossil.FslID
			if err := cloned.DB().QueryRow("SELECT rid FROM blob WHERE uuid=? AND size>=0", final).Scan(&rid); err != nil {
				t.Fatalf("final in-cone artifact missing: %v", err)
			}
			if data, err := content.Expand(cloned.DB(), rid); err != nil || string(data) != "final in-cone content" {
				t.Fatalf("final content=%q err=%v", data, err)
			}
			if err := cloned.DB().QueryRow("SELECT rid FROM blob WHERE uuid=? AND size>=0", excluded).Scan(&rid); err != sql.ErrNoRows {
				t.Fatalf("excluded content held: %v", err)
			}
			pulled := setupSyncTestRepo(t)
			if err := content.SetStoreCone(pulled.DB(), c); err != nil {
				t.Fatal(err)
			}
			before := transport.applied
			if _, err := Sync(context.Background(), pulled, transport, SyncOpts{Pull: true, ProjectCode: testPC}); err != nil {
				t.Fatalf("real pull: %v", err)
			}
			if transport.applied <= before {
				t.Fatal("real pull never selected prepared path")
			}
			if err := pulled.DB().QueryRow("SELECT rid FROM blob WHERE uuid=? AND size>=0", final).Scan(&rid); err != nil {
				t.Fatalf("real pull final content missing: %v", err)
			}
		})
	}
}

func TestPreparedSparseServiceFallback(t *testing.T) {
	r := setupSyncTestRepo(t)
	r.DB().SqlDB().SetMaxOpenConns(1)
	service, err := prepareSparseService(context.Background(), r, HandleOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	cards := []xfer.Card{versionPragma("client-version"), sendSubmanifests(),
		&xfer.PragmaCard{Name: "cone", Values: []string{"src"}},
		&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true}}
	if _, prepared, err := service.handle(context.Background(), &xfer.Message{Cards: cards}); err != nil || !prepared {
		t.Fatalf("prepared request: applied=%v err=%v", prepared, err)
	}
	if _, prepared, err := service.handle(context.Background(), &xfer.Message{Cards: cards}); err != nil || !prepared {
		t.Fatalf("second prepared request: applied=%v err=%v", prepared, err)
	}
	storeTestBlob(t, r, []byte("generation change"))
	if _, prepared, err := service.handle(context.Background(), &xfer.Message{Cards: cards}); err != nil || prepared {
		t.Fatalf("stale request: applied=%v err=%v", prepared, err)
	}
}
