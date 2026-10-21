package sync

import (
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// A cone request keeps the live shun and private gates: an in-cone check-in
// naming both artifacts still never puts the shunned basis on the wire, as
// payload or as a delta source, while its public dependent goes out.
func TestSparseWalkDoesNotSendShunnedContent(t *testing.T) {
	for _, private := range []bool{false, true} {
		for _, requested := range []bool{false, true} {
			t.Run(fmt.Sprintf("private=%v/gimme=%v", private, requested), func(t *testing.T) {
				r, baseUUID, targetUUID := shunSendFixture(t, private)
				data, err := (&deck.Deck{C: "names both", D: time.Now().UTC(), U: deck.User("u"),
					F: []deck.FileCard{{Name: "src/base", UUID: baseUUID}, {Name: "src/target", UUID: targetUUID}}}).Marshal()
				if err != nil {
					t.Fatal(err)
				}
				storeAndLink(t, r, data)
				if _, err := r.DB().Exec("INSERT INTO shun(uuid,mtime) VALUES(?,0)", baseUUID); err != nil {
					t.Fatal(err)
				}
				cards := append([]xfer.Card{&xfer.PragmaCard{Name: "cone", Values: []string{"src"}}},
					shunSendCards(private, requested, baseUUID, targetUUID)...)
				assertShunSend(t, shunWireRound(t, r, cards), baseUUID, targetUUID)
			})
		}
	}
}

// A private in-cone file stays off a public clone's wire: the walk reaches
// it but serves only what the clone may carry.
func TestSparseWalkWithholdsPrivateFromPublicClone(t *testing.T) {
	r := setupSyncTestRepo(t)
	secret := storeAndLink(t, r, []byte("private in-cone content"))
	public := storeAndLink(t, r, []byte("public in-cone content"))
	if _, err := r.DB().Exec("INSERT INTO private(rid) SELECT rid FROM blob WHERE uuid=?", secret); err != nil {
		t.Fatal(err)
	}
	data, err := (&deck.Deck{C: "private walk", D: time.Now().UTC(), U: deck.User("u"),
		F: []deck.FileCard{{Name: "src/public", UUID: public}, {Name: "src/secret", UUID: secret}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	storeAndLink(t, r, data)
	resp := shunWireRound(t, r, []xfer.Card{versionPragma("client-version"),
		&xfer.PragmaCard{Name: "cone", Values: []string{"src"}},
		&xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true}})
	if shunHasFile(resp, secret) {
		t.Fatal("private payload on a public clone's wire")
	}
	if !shunHasFile(resp, public) {
		t.Fatal("public in-cone file withheld")
	}
}
