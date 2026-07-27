package sync

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"strings"

	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// computeLogin produces a LoginCard for the given credentials.
// payload is the encoded bytes of all non-login cards (including random comment).
//
// The nonce is the SHA1 of that payload and the signature is the SHA1 of the
// nonce text followed by the *hashed* password — never the cleartext one. The
// server compares against the hash it stores in user.pw, so a login card built
// from the cleartext password is rejected exactly as a wrong password is
// (src/http.c http_build_login_card, src/xfer.c check_login).
func computeLogin(user, password, projectCode string, payload []byte) *xfer.LoginCard {
	if user == "" {
		panic("sync.computeLogin: user must not be empty")
	}
	if payload == nil {
		panic("sync.computeLogin: payload must not be nil")
	}
	nonce := sha1Hex(payload)
	signature := sha1Hex([]byte(nonce + sharedSecret(password, user, projectCode)))
	return &xfer.LoginCard{User: user, Nonce: nonce, Signature: signature}
}

// sharedSecret derives the password hash the server holds in user.pw, which is
// what a login signature is computed over.
//
// Canonical is src/sha1.c sha1_shared_secret: SHA1 of
// "<project-code>/<login>/<password>". The project code salts it, so the same
// password yields a different secret per repository. When the project code is
// not yet known — the first request of a clone — canonical uses the cleartext
// password, "since that is all we have"; the server's check_login has a
// matching branch for repositories that store cleartext passwords.
//
// A password that is already 40 hex characters is taken to be a hash and used
// as-is, matching http_build_login_card. That is how a caller can hand us the
// stored secret instead of a cleartext password, and it is the same heuristic
// (and the same limitation for a cleartext password that looks like a hash)
// canonical applies.
func sharedSecret(password, login, projectCode string) string {
	if isSHA1Hex(password) {
		return password
	}
	if projectCode == "" {
		return password
	}
	return sha1Hex([]byte(projectCode + "/" + login + "/" + password))
}

func isSHA1Hex(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// randomComment returns a comment card holding fresh randomness, to be sent as
// the last card before a login card is prepended.
//
// The nonce hashes every byte after the login card and the server re-hashes
// those same bytes to check it (src/xfer.c check_tail_hash), so this card must
// be part of the message, not merely part of the hash — hashing a comment that
// is never transmitted yields a nonce the server can never reproduce, and it
// rejects the login before the password is ever compared.
func randomComment(rng simio.Rand) *xfer.CommentCard {
	rb := make([]byte, 20)
	rng.Read(rb)
	return &xfer.CommentCard{Text: strings.ToUpper(hex.EncodeToString(rb))}
}

// encodeCards returns the wire bytes of cards, which is what a login card's
// nonce is computed over.
func encodeCards(cards []xfer.Card) ([]byte, error) {
	var buf bytes.Buffer
	for _, c := range cards {
		if err := xfer.EncodeCard(&buf, c); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}
