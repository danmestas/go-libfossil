package xfer

import (
	"net/url"
	"strings"

	"github.com/danmestas/go-libfossil/internal/deck"
	"github.com/danmestas/go-libfossil/internal/hash"
)

// LoginCookieName is the HTTP cookie in which a client may carry its login
// card instead of the body, once the server's release accepts it (§6.5).
const LoginCookieName = "x-f-l-c"

// DecodeRequest decodes a sync request as Decode does. When loginCookie, the
// value of the request's LoginCookieName cookie, holds a login card, that card
// is put first, its nonce bound to the whole decompressed body: fossil's
// page_xfer handles a cookie login as a login line ahead of the body, so the
// "rest of the message" its nonce covers is all of it (§6.5, check_tail_hash).
// A cookie that is not a login card is ignored, as page_xfer ignores it.
func DecodeRequest(data []byte, contentType, loginCookie string) (*Message, error) {
	raw := data
	if len(data) > 0 && contentTypeIsCompressed(contentType) {
		var err error
		if raw, err = decompressContainer(data); err != nil {
			return nil, err
		}
	}
	msg, err := DecodeUncompressed(raw)
	if err != nil {
		return nil, err
	}
	if lc := parseLoginCookie(loginCookie); lc != nil {
		lc.tailSHA1 = hash.SHA1(raw)
		msg.Cards = append([]Card{lc}, msg.Cards...)
	}
	return msg, nil
}

// SplitLoginCookie returns the message to send as the body and the
// LoginCookieName cookie value to send beside it. When m is LoginCookieOK and
// opens with a login card, that card leaves the body for the cookie. Its
// nonce, computed over the cards after it, then covers the whole body, as a
// cookie login's must (§6.5). Otherwise m goes as it is, with no cookie.
func (m *Message) SplitLoginCookie() (*Message, string) {
	if !m.LoginCookieOK || len(m.Cards) == 0 {
		return m, ""
	}
	lc, ok := m.Cards[0].(*LoginCard)
	if !ok {
		return m, ""
	}
	line := "login " + deck.FossilEncode(lc.User) + " " + lc.Nonce + " " + lc.Signature
	return &Message{Cards: m.Cards[1:]}, url.QueryEscape(line)
}

// parseLoginCookie reads a login card from a cookie value: URL-encoded, as
// fossil's %T writes it, then "login USER NONCE SIGNATURE" split on white
// space, as page_xfer tokenizes it. It returns nil for anything else.
func parseLoginCookie(v string) *LoginCard {
	if v == "" {
		return nil
	}
	line, err := url.QueryUnescape(v)
	if err != nil {
		return nil
	}
	tokens := strings.Fields(line)
	if len(tokens) != 4 || tokens[0] != "login" {
		return nil
	}
	return &LoginCard{
		User: deck.FossilDecode(tokens[1]), Nonce: tokens[2], Signature: tokens[3],
	}
}
