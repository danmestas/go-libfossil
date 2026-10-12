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
