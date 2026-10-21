package sync

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// A gimme for a private artifact inside the cone must get the reply the dense
// handler gives: the §8.6 private notice to a dated peer, nothing to an
// undated one, and never the payload.
func TestPreparedSparseServicePrivateGimmeMatchesDense(t *testing.T) {
	cases := []struct {
		name     string
		caps     string
		path     string
		dated    bool
		shunned  bool
		wantNote bool
	}{
		{name: "in-cone", caps: "gjorz", path: "src/secret", dated: true, wantNote: true},
		{name: "in-cone/private-cap", caps: "gjorzx", path: "src/secret", dated: true, wantNote: true},
		{name: "in-cone/shunned", caps: "gjorz", path: "src/secret", dated: true, shunned: true, wantNote: true},
		{name: "in-cone/undated", caps: "gjorz", path: "src/secret"},
		// §6.4: a gimme names its artifact, so the cone does not filter it.
		{name: "outside-cone", caps: "gjorz", path: "zzz/secret", dated: true, wantNote: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := setupSyncTestRepo(t)
			if _, err := r.DB().Exec("UPDATE user SET cap=? WHERE login='nobody'", tc.caps); err != nil {
				t.Fatal(err)
			}
			secret := storeAndLink(t, r, []byte("private in-cone content"))
			public := storeAndLink(t, r, []byte("public content"))
			data, err := (&deck.Deck{C: "private gimme", D: time.Now().UTC(), U: deck.User("u"),
				F: []deck.FileCard{{Name: "src/public", UUID: public}, {Name: tc.path, UUID: secret}}}).Marshal()
			if err != nil {
				t.Fatal(err)
			}
			storeAndLink(t, r, data)
			if _, err := r.DB().Exec("INSERT INTO private(rid) SELECT rid FROM blob WHERE uuid=?", secret); err != nil {
				t.Fatal(err)
			}
			if tc.shunned {
				if _, err := r.DB().Exec("INSERT INTO shun(uuid,mtime) VALUES(?,0)", secret); err != nil {
					t.Fatal(err)
				}
			}
			service, err := prepareSparseService(context.Background(), r, HandleOpts{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			var cards []xfer.Card
			if tc.dated {
				cards = append(cards, versionPragma("client-version"))
			}
			cards = append(cards, &xfer.PragmaCard{Name: "cone", Values: []string{"src"}},
				&xfer.PullCard{ServerCode: "test", ProjectCode: testPC},
				&xfer.GimmeCard{UUID: secret})
			req := &xfer.Message{Cards: cards}
			prepared, applied, err := service.handle(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !applied {
				t.Fatal("prepared path not selected")
			}
			dense, err := HandleSync(context.Background(), r, req)
			if err != nil {
				t.Fatal(err)
			}
			preparedWire := wireWithoutComments(t, prepared)
			denseWire := wireWithoutComments(t, dense)
			if !bytes.Equal(preparedWire, denseWire) {
				t.Fatalf("prepared reply differs from dense\nprepared:\n%s\ndense:\n%s", preparedWire, denseWire)
			}
			note := false
			for _, card := range prepared.Cards {
				switch c := card.(type) {
				case *xfer.IGotCard:
					if c.UUID == secret {
						if !c.IsPrivate {
							t.Fatal("private artifact announced as public")
						}
						note = true
					}
				case *xfer.FileCard:
					if c.UUID == secret {
						t.Fatal("private payload emitted")
					}
				case *xfer.CFileCard:
					if c.UUID == secret {
						t.Fatal("private payload emitted")
					}
				case *xfer.PrivateCard:
					t.Fatal("private card emitted outside a private sync")
				}
			}
			if note != tc.wantNote {
				t.Fatalf("private notice=%v, want %v; cards %v", note, tc.wantNote, prepared.Cards)
			}
		})
	}
}

// The closing timestamp comment can straddle a second between two replies.
func wireWithoutComments(t *testing.T, m *xfer.Message) []byte {
	t.Helper()
	cards := make([]xfer.Card, 0, len(m.Cards))
	for _, card := range m.Cards {
		if _, ok := card.(*xfer.CommentCard); !ok {
			cards = append(cards, card)
		}
	}
	wire, err := (&xfer.Message{Cards: cards}).EncodeUncompressed()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// Shunned and private artifacts present at preparation stay in the index;
// the live gates must still withhold the shunned payload and basis.
func TestPreparedSparseServiceShunPrepared(t *testing.T) {
	for _, private := range []bool{false, true} {
		for _, requested := range []bool{false, true} {
			t.Run(fmt.Sprintf("private=%v/gimme=%v", private, requested), func(t *testing.T) {
				r, baseUUID, targetUUID := shunSendFixture(t, private)
				if _, err := r.DB().Exec("INSERT INTO shun(uuid,mtime) VALUES(?,0)", baseUUID); err != nil {
					t.Fatal(err)
				}
				service, err := prepareSparseService(context.Background(), r, HandleOpts{})
				if err != nil {
					t.Fatal(err)
				}
				defer service.Close()
				cards := []xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{"src"}}}
				cards = append(cards, shunSendCards(private, requested, baseUUID, targetUUID)...)
				resp, _, err := service.handle(context.Background(), &xfer.Message{Cards: cards})
				if err != nil {
					t.Fatal(err)
				}
				assertShunSend(t, resp, baseUUID, targetUUID)
			})
		}
	}
}
