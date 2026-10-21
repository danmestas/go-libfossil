package sync

import (
	"context"
	"testing"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

func shunWireRound(t *testing.T, r *repo.Repo, cards []xfer.Card) *xfer.Message {
	t.Helper()
	wire, err := (&xfer.Message{Cards: cards}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	req, err := xfer.Decode(wire, xfer.ContentTypeCompressed)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := HandleSync(context.Background(), r, req)
	if err != nil {
		t.Fatal(err)
	}
	wire, err = resp.Encode()
	if err != nil {
		t.Fatal(err)
	}
	resp, err = xfer.Decode(wire, xfer.ContentTypeCompressed)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Shun enforcement must not weaken either the ordinary private boundary or
// the requirement for an authorized pull/clone before answering a gimme.
func TestShunSendPreservesAuthorization(t *testing.T) {
	for _, requested := range []bool{false, true} {
		r, baseUUID, targetUUID := shunSendFixture(t, true)
		resp := shunWireRound(t, r, shunSendCards(false, requested, baseUUID, targetUUID))
		assertShunSend(t, resp, baseUUID, targetUUID)
		resp = shunWireRound(t, r, []xfer.Card{&xfer.GimmeCard{UUID: targetUUID}})
		if shunHasFile(resp, targetUUID) {
			t.Fatal("gimme without authorized pull disclosed content")
		}
		_, err := r.DB().Exec("UPDATE user SET cap='' WHERE login IN ('nobody','anonymous')")
		if err != nil {
			t.Fatal(err)
		}
		resp = shunWireRound(t, r, shunSendCards(false, requested, baseUUID, targetUUID))
		if shunHasFile(resp, targetUUID) {
			t.Fatal("unauthorized clone/pull disclosed content")
		}
	}
}

func shunHasFile(resp *xfer.Message, uuid string) bool {
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
