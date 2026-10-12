package auth

import (
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

var ErrAuthFailed = errors.New("authentication failed")

// VerifyLogin validates a login card against the user table, as fossil's
// check_tail_hash and check_login do (src/xfer.c, draft §6.4).
//
// The card must have been decoded from the request it arrived in: its nonce
// must be the SHA-1 of the body that followed it (xfer.LoginCard
// NonceCoversTail). The signature covers only the nonce, so without that
// check a card captured from one request authenticates any other.
//
// nobody and anonymous succeed without a password and carry no capabilities
// of their own. Any other user needs a non-empty stored password: an empty one
// would make the signature SHA1(nonce), which anyone can compute.
//
// Returns the authenticated User on success, or ErrAuthFailed on any failure.
// Panics if d or card is nil (TigerStyle preconditions).
func VerifyLogin(d *db.DB, projectCode string, card *xfer.LoginCard) (User, error) {
	if d == nil {
		panic("auth.VerifyLogin: d must not be nil")
	}
	if projectCode == "" {
		panic("auth.VerifyLogin: projectCode must not be empty")
	}
	if card == nil {
		panic("auth.VerifyLogin: card must not be nil")
	}
	if !card.NonceCoversTail() {
		return User{}, ErrAuthFailed
	}
	if card.User == "nobody" || card.User == "anonymous" {
		return User{Login: card.User}, nil
	}

	// 1. Look up user by card.User and retrieve stored password + cexpire.
	// The reserved logins are capability templates, never accounts.
	var storedPW string
	var cexpire sql.NullString
	var u User
	var info sql.NullString
	var mtimeRaw any

	// An account with no stored password cannot be logged into, as in
	// fossil's check_login (src/xfer.c, length(pw)>0): with an empty secret
	// the signature is SHA1(nonce), which anyone can compute. A new
	// repository's creator starts that way, holding Setup.
	err := d.QueryRow(
		"SELECT uid, login, pw, cap, cexpire, info, mtime FROM user WHERE login=?"+
			" AND login NOT IN ('anonymous','nobody','developer','reader')"+
			" AND length(pw)>0",
		card.User,
	).Scan(&u.UID, &u.Login, &storedPW, &u.Cap, &cexpire, &info, &mtimeRaw)

	// 2. If not found → ErrAuthFailed
	if err != nil {
		return User{}, ErrAuthFailed
	}

	if info.Valid {
		u.Info = info.String
	}

	// 3. If cexpire is set and in the past → ErrAuthFailed
	if cexpire.Valid {
		expireTime, ok := parseCExpire(cexpire.String)
		if !ok || time.Now().After(expireTime) {
			return User{}, ErrAuthFailed
		}
		u.CExpire = expireTime
	}

	// 4. Check the signature against the stored password.
	if !signatureMatches(card, storedPW, projectCode) {
		return User{}, ErrAuthFailed
	}
	return u, nil
}

// signatureMatches reports whether card's signature is SHA1(nonce + secret)
// for the stored password, as fossil's check_login decides it. The stored
// password is normally the shared secret, SHA1(projectCode/login/password).
// One that is not 40 characters is a password stored in clear, so the
// signature is checked again against the shared secret it derives (§6.4).
// Both comparisons run in constant time.
func signatureMatches(card *xfer.LoginCard, storedPW, projectCode string) bool {
	sig := []byte(card.Signature)
	if subtle.ConstantTimeCompare([]byte(sha1hex([]byte(card.Nonce+storedPW))), sig) == 1 {
		return true
	}
	if len(storedPW) == 40 {
		return false
	}
	secret := hashPassword(projectCode, card.User, storedPW)
	return subtle.ConstantTimeCompare([]byte(sha1hex([]byte(card.Nonce+secret))), sig) == 1
}

// parseCExpire parses a user.cexpire value. SQLite may return either ISO 8601
// or "2006-01-02 15:04:05". An expiry that does not parse reports !ok, which
// the caller treats as expired for safety.
func parseCExpire(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t, err = time.Parse("2006-01-02 15:04:05", s)
	}
	return t, err == nil
}

func sha1hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}
