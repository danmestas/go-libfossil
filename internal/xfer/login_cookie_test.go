package xfer

import (
	"testing"

	"github.com/danmestas/go-libfossil/internal/hash"
)

// TestDecodeRequestLoginCookie pins §6.5: a login card carried in the
// x-f-l-c cookie is put first, with its nonce bound to the whole body, and a
// cookie that is not a login card is ignored.
func TestDecodeRequestLoginCookie(t *testing.T) {
	body := []byte("pull sc pc\n# trailing comment\n")
	nonce := hash.SHA1(body)
	msg, err := DecodeRequest(body, ContentTypeUncompressed, "login+alice+"+nonce+"+sig")
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	lc, ok := msg.Cards[0].(*LoginCard)
	if !ok {
		t.Fatalf("first card is %T, want the cookie's login card", msg.Cards[0])
	}
	if lc.User != "alice" || lc.Signature != "sig" || !lc.NonceCoversTail() {
		t.Fatalf("cookie login = %+v, covers body %v", lc, lc.NonceCoversTail())
	}

	notLogins := []string{"", "alice+" + nonce + "+sig", "login+alice+" + nonce, "%zz"}
	for _, cookie := range notLogins {
		msg, err := DecodeRequest(body, ContentTypeUncompressed, cookie)
		if err != nil {
			t.Fatalf("DecodeRequest(cookie %q): %v", cookie, err)
		}
		if _, ok := msg.Cards[0].(*LoginCard); ok {
			t.Errorf("cookie %q was taken as a login card", cookie)
		}
	}
}

// TestSplitLoginCookieRoundTrip pins that a login moved into the cookie is
// read back by DecodeRequest with its nonce covering the body it left, and
// that a message not marked LoginCookieOK keeps its login in the body.
func TestSplitLoginCookieRoundTrip(t *testing.T) {
	rest := []Card{&PullCard{ServerCode: "sc", ProjectCode: "pc"}, &CommentCard{Text: "ABC"}}
	tail, err := (&Message{Cards: rest}).EncodeUncompressed()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	login := &LoginCard{User: "a b", Nonce: hash.SHA1(tail), Signature: "sig"}
	m := &Message{Cards: append([]Card{login}, rest...), LoginCookieOK: true}

	sent, cookie := m.SplitLoginCookie()
	if cookie == "" || len(sent.Cards) != len(rest) {
		t.Fatalf("split = %d cards, cookie %q; want the login moved out",
			len(sent.Cards), cookie)
	}
	body, err := sent.EncodeUncompressed()
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	got, err := DecodeRequest(body, ContentTypeUncompressed, cookie)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	lc, ok := got.Cards[0].(*LoginCard)
	if !ok || lc.User != "a b" || !lc.NonceCoversTail() {
		t.Fatalf("decoded login = %+v; want user %q covering the body", got.Cards[0], "a b")
	}

	m.LoginCookieOK = false
	if sent, cookie := m.SplitLoginCookie(); cookie != "" || sent != m {
		t.Fatalf("unmarked message split: cookie %q", cookie)
	}
}
