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

// VerifyLogin validates a login card against the user table.
// The server does NOT need the raw payload — the nonce is in card.Nonce.
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

	// 1. Look up user by card.User and retrieve stored password + cexpire
	var storedPW string
	var cexpire sql.NullString
	var u User
	var info sql.NullString
	var mtimeRaw any

	err := d.QueryRow(
		"SELECT uid, login, pw, cap, cexpire, info, mtime FROM user WHERE login=?",
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
		// Parse cexpire - SQLite may return either "2006-01-02 15:04:05" or ISO 8601 format
		var expireTime time.Time
		var err error

		// Try ISO 8601 first (SQLite's preferred format)
		expireTime, err = time.Parse(time.RFC3339, cexpire.String)
		if err != nil {
			// Fall back to space-separated format
			expireTime, err = time.Parse("2006-01-02 15:04:05", cexpire.String)
			if err != nil {
				// If we can't parse the expiry, treat it as expired for safety
				return User{}, ErrAuthFailed
			}
		}

		// Check if expired
		if time.Now().After(expireTime) {
			return User{}, ErrAuthFailed
		}

		u.CExpire = expireTime
	}

	// 4. Recompute: expected = SHA1(card.Nonce + storedPW)
	// storedPW is already SHA1(projectCode/login/password) from the user table
	expected := sha1hex([]byte(card.Nonce + storedPW))

	// 5. Compare using crypto/subtle.ConstantTimeCompare
	expectedBytes := []byte(expected)
	signatureBytes := []byte(card.Signature)

	// 6. If mismatch → ErrAuthFailed
	if subtle.ConstantTimeCompare(expectedBytes, signatureBytes) != 1 {
		return User{}, ErrAuthFailed
	}

	// 7. Return User
	return u, nil
}

func sha1hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}
