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
//
// payload must be exactly the bytes that will follow the login card on the
// wire: the nonce is "SHA-1 over every decompressed body byte after the body
// login line's LF through body end" (§6.2), and the server recomputes it from
// the bytes it received (src/xfer.c check_tail_hash). Hashing anything the
// message does not carry yields a nonce the server cannot reproduce, and it
// rejects the login before the password is compared at all — which is why
// #203 failed identically for right and wrong passwords.
//
//	SIGNATURE = SHA1(NONCE-hex || shared-secret-hex)
//
// — §6.4: the two 40-character hex strings concatenated, not their raw digests.
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

// sharedSecret derives the value a login signature is computed over, which is
// also what the server stores in user.pw.
//
//	shared-secret = SHA1(project-code "/" login-name "/" plaintext-password)
//
// — §6.3, 40 lowercase hex. The project code salts it, so the same password
// yields a different secret per repository; deriving it from the wrong code is
// indistinguishable from a wrong password (#203).
//
// Two branches canonical's C has are deliberately absent, because §6.3 states
// neither is reachable on the wire: the cleartext-password fallback for an
// unknown project code (src/sha1.c) cannot arise because "the initial clone
// sends no login card", and http_build_login_card's rule that an already
// 40-hex password is passed through unhashed serves fossil's own cached
// last-sync-pw, which this library does not keep. Both are silent behavioural
// forks on the authentication path, so the spec's narrower contract is the one
// implemented here; a caller with no project code is a caller error, not a
// wire case.
func sharedSecret(password, login, projectCode string) string {
	if projectCode == "" {
		panic("sync.sharedSecret: projectCode must not be empty")
	}
	return sha1Hex([]byte(projectCode + "/" + login + "/" + password))
}

// randomComment returns the nonce comment, sent as the last card before a
// login card is prepended. "Random comments vary independently generated
// nonces" (§6.4); without it two identical requests would share a nonce and
// so a signature.
//
// Its grammar is nonce-comment = "#" SP 40HEXDIG-UC LF (§B.3) — 20 random
// bytes as uppercase hex.
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
