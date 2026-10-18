package auth

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/go-libfossil/db"
)

type InviteToken struct {
	URL      string `json:"url"`
	Login    string `json:"login"`
	Password string `json:"password"`
	Caps     string `json:"caps"`
}

func (t InviteToken) Encode() string {
	b, err := json.Marshal(t)
	if err != nil {
		panic(fmt.Sprintf("auth.InviteToken.Encode: %v", err))
	}
	return base64.URLEncoding.EncodeToString(b)
}

func DecodeInviteToken(s string) (InviteToken, error) {
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return InviteToken{}, fmt.Errorf("auth.DecodeInviteToken: %w", err)
	}
	var t InviteToken
	if err := json.Unmarshal(b, &t); err != nil {
		return InviteToken{}, fmt.Errorf("auth.DecodeInviteToken: %w", err)
	}
	if t.Login == "" {
		return InviteToken{}, fmt.Errorf("auth.DecodeInviteToken: missing login")
	}
	if t.Password == "" {
		return InviteToken{}, fmt.Errorf("auth.DecodeInviteToken: missing password")
	}
	return t, nil
}

// InviteExpiryKey is the config row that holds login's invite expiry. It is
// go-libfossil's own: fossil never writes it, unlike user.cexpire, which is
// fossil's web-login cookie expiry (#441).
func InviteExpiryKey(login string) string {
	if login == "" {
		panic("auth.InviteExpiryKey: login must not be empty")
	}
	return "invite-expire:" + login
}

// SetInviteExpiry records the instant after which login's invite no longer
// admits a sync login, in UTC.
func SetInviteExpiry(d *db.DB, login string, expiry time.Time) error {
	if d == nil {
		panic("auth.SetInviteExpiry: d must not be nil")
	}
	_, err := d.Exec(
		"INSERT OR REPLACE INTO config(name, value, mtime) VALUES(?, ?, strftime('%s','now'))",
		InviteExpiryKey(login), expiry.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("set invite expiry for %q: %w", login, err)
	}
	return nil
}

// inviteExpired reports whether login has an invite expiry that has passed.
// An expiry that does not read as a time counts as passed, for safety.
func inviteExpired(q db.Querier, login string, now time.Time) (bool, error) {
	var value string
	err := q.QueryRow("SELECT value FROM config WHERE name=?", InviteExpiryKey(login)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read invite expiry for %q: %w", login, err)
	}
	expiry, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return true, nil
	}
	return !now.Before(expiry), nil
}
