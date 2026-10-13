package sync

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// TestClientAnswersPrivateGimme pins the client side of §8.6 and §3.7 (c):
// outside a private sync, a gimme for private content is answered with igot
// HASH 1 when the server dates itself 2020-04-13 or later, and that answer
// to the first reply continues the session once. An older or undated server
// is told nothing, and the session ends.
func TestClientAnswersPrivateGimme(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version []string
		answer  bool
	}{
		{"recent", []string{"22800", "20260311", "113146"}, true},
		{"old", []string{"22800", "20200101", "000000"}, false},
		{"undated", []string{"22800"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := newTestSession(t, SyncOpts{Push: true, ProjectCode: testPC})
			rid, uuid, err := blob.Store(r.DB(), []byte("held as private\n"))
			if err != nil {
				t.Fatal(err)
			}
			if err := content.MakePrivate(r.DB(), int64(rid)); err != nil {
				t.Fatal(err)
			}
			reply := &xfer.Message{Cards: []xfer.Card{
				&xfer.PragmaCard{Name: "server-version", Values: tc.version},
				&xfer.GimmeCard{UUID: uuid},
			}}
			done, err := s.processResponse(context.Background(), reply)
			if err != nil {
				t.Fatal(err)
			}
			if done == tc.answer {
				t.Fatalf("converged %v, want %v", done, !tc.answer)
			}
			next, err := s.buildRequest(1)
			if err != nil {
				t.Fatal(err)
			}
			got := 0
			for _, c := range next.Cards {
				switch c := c.(type) {
				case *xfer.IGotCard:
					if c.UUID == uuid && c.IsPrivate {
						got++
					}
				case *xfer.FileCard:
					t.Fatalf("private content sent: %s", c.UUID)
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.answer]; got != want {
				t.Fatalf("%d igot %s 1 cards, want %d", got, uuid, want)
			}
		})
	}
}
