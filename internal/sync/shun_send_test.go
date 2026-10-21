package sync

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/blob"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// Exercise actual wire cards, including a dependent whose stored delta names
// the shunned source. Dense serving must not send that source, even when
// private sync is authorized.
func TestSyncDoesNotSendShunnedContent(t *testing.T) {
	for _, private := range []bool{false, true} {
		for _, requested := range []bool{false, true} {
			t.Run(fmt.Sprintf("private=%v/gimme=%v", private, requested), func(t *testing.T) {
				r, baseUUID, targetUUID := shunSendFixture(t, private)
				if _, err := r.DB().Exec("INSERT INTO shun(uuid,mtime) VALUES(?,0)", baseUUID); err != nil {
					t.Fatal(err)
				}
				cards := shunSendCards(private, requested, baseUUID, targetUUID)
				resp := shunWireRound(t, r, cards)
				assertShunSend(t, resp, baseUUID, targetUUID)
			})
		}
	}
}

func shunSendFixture(t *testing.T, private bool) (*repo.Repo, string, string) {
	t.Helper()
	r := setupSyncTestRepo(t)
	if _, err := r.DB().Exec("UPDATE user SET cap='gox' WHERE login='nobody'"); err != nil {
		t.Fatal(err)
	}
	base := bytes.Repeat([]byte("0123456789abcdefghijklmnopqrstuv"), 2048)
	target := append([]byte(nil), base...)
	target[len(target)/2] = 'X'
	baseRID, baseUUID, err := blob.Store(r.DB(), base)
	if err != nil {
		t.Fatal(err)
	}
	targetRID, targetUUID, err := blob.Store(r.DB(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WithTx(func(tx *db.Tx) error {
		saved, err := content.Deltify(tx, targetRID, baseRID)
		if err == nil && saved == 0 {
			t.Fatal("fixture did not store a delta")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if private {
		if _, err := r.DB().Exec("INSERT INTO private(rid) VALUES(?)", baseRID); err != nil {
			t.Fatal(err)
		}
	}
	return r, baseUUID, targetUUID
}

func shunSendCards(private, requested bool, baseUUID, targetUUID string) []xfer.Card {
	cards := []xfer.Card{versionPragma("client-version")}
	if private {
		cards = append(cards, &xfer.PragmaCard{Name: "send-private"})
	}
	if requested {
		return append(cards, &xfer.PullCard{ServerCode: "test", ProjectCode: testPC},
			&xfer.GimmeCard{UUID: baseUUID}, &xfer.GimmeCard{UUID: targetUUID})
	}
	return append(cards, &xfer.CloneCard{Version: 3, SeqNo: 1, SeqNoIsDecimal: true})
}

func assertShunSend(t *testing.T, resp *xfer.Message, baseUUID, targetUUID string) {
	t.Helper()
	if shunHasFile(resp, baseUUID) {
		t.Fatal("shunned payload emitted on wire")
	}
	if !shunHasFile(resp, targetUUID) {
		t.Fatal("public dependent withheld")
	}
	for _, card := range resp.Cards {
		switch c := card.(type) {
		case *xfer.FileCard:
			if c.DeltaSrc == baseUUID {
				t.Fatal("file names shunned basis")
			}
		case *xfer.CFileCard:
			if c.DeltaSrc == baseUUID {
				t.Fatal("cfile names shunned basis")
			}
		}
	}
}
