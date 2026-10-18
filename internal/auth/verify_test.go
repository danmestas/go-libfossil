package auth

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// buildLoginCard returns the login card a client would send ahead of payload,
// as the server sees it: decoded from the wire, so it carries the body tail
// its nonce is checked against. payload is the body after the login line.
func buildLoginCard(user, password, projectCode string, payload []byte) *xfer.LoginCard {
	nonce := sha1Hex(payload)
	sharedSecret := sha1Hex([]byte(projectCode + "/" + user + "/" + password))
	signature := sha1Hex([]byte(nonce + sharedSecret))
	return onWire(&xfer.LoginCard{User: user, Nonce: nonce, Signature: signature}, payload)
}

// onWire decodes card followed by body, as a server receives them.
func onWire(card *xfer.LoginCard, body []byte) *xfer.LoginCard {
	var buf bytes.Buffer
	if err := xfer.EncodeCard(&buf, card); err != nil {
		panic(err)
	}
	buf.Write(body)
	msg, err := xfer.DecodeUncompressed(buf.Bytes())
	if err != nil {
		panic(err)
	}
	return msg.Cards[0].(*xfer.LoginCard)
}

func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}

func TestVerifyLoginSuccess(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	// Create user with known password
	err := CreateUser(d, code, "alice", "secret123", "ios")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Build valid login card
	payload := []byte("push sc pc\nigot abc123\n")
	card := buildLoginCard("alice", "secret123", code, payload)

	// Verify should succeed
	user, err := VerifyLogin(d, code, card)
	if err != nil {
		t.Fatalf("VerifyLogin: expected success, got %v", err)
	}

	if user.Login != "alice" {
		t.Errorf("user.Login = %q, want alice", user.Login)
	}
	if user.Cap != "ios" {
		t.Errorf("user.Cap = %q, want ios", user.Cap)
	}
}

func TestVerifyLoginWrongPassword(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	// Create user with known password
	err := CreateUser(d, code, "alice", "secret123", "o")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Build login card with wrong password
	payload := []byte("push sc pc\nigot abc123\n")
	card := buildLoginCard("alice", "wrongpassword", code, payload)

	// Verify should fail
	_, err = VerifyLogin(d, code, card)
	if err != ErrAuthFailed {
		t.Errorf("VerifyLogin wrong password: got %v, want ErrAuthFailed", err)
	}
}

func TestVerifyLoginUnknownUser(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	// Build login card for nonexistent user
	payload := []byte("push sc pc\nigot abc123\n")
	card := buildLoginCard("doesnotexist", "anypassword", code, payload)

	// Verify should fail
	_, err := VerifyLogin(d, code, card)
	if err != ErrAuthFailed {
		t.Errorf("VerifyLogin unknown user: got %v, want ErrAuthFailed", err)
	}
}

func TestVerifyLoginNilDB(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("VerifyLogin with nil DB should panic")
		}
	}()

	card := &xfer.LoginCard{User: "alice", Nonce: "abc", Signature: "def"}
	VerifyLogin(nil, "test-code", card)
}

func TestVerifyLoginNilCard(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	defer func() {
		if r := recover(); r == nil {
			t.Error("VerifyLogin with nil card should panic")
		}
	}()

	VerifyLogin(d, code, nil)
}

// A login card's signature covers only its nonce, so a card captured from one
// request must fail once it heads any other body (draft §6.2).
func TestVerifyLoginReplayedOntoOtherBody(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)
	if err := CreateUser(d, code, "alice", "secret123", "ios"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	signed := buildLoginCard("alice", "secret123", code, []byte("pull sc pc\n"))
	replay := onWire(&xfer.LoginCard{
		User: signed.User, Nonce: signed.Nonce, Signature: signed.Signature,
	}, []byte("push sc pc\n"))
	if _, err := VerifyLogin(d, code, replay); err != ErrAuthFailed {
		t.Fatalf("VerifyLogin replayed card: got %v, want ErrAuthFailed", err)
	}
}

// A card that never came off the wire has no body for its nonce to cover.
func TestVerifyLoginUndecodedCard(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)
	if err := CreateUser(d, code, "alice", "secret123", "ios"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	payload := []byte("pull sc pc\n")
	nonce := sha1Hex(payload)
	secret := sha1Hex([]byte(code + "/alice/secret123"))
	card := &xfer.LoginCard{User: "alice", Nonce: nonce, Signature: sha1Hex([]byte(nonce + secret))}
	if _, err := VerifyLogin(d, code, card); err != ErrAuthFailed {
		t.Fatalf("VerifyLogin undecoded card: got %v, want ErrAuthFailed", err)
	}
}

// nobody and anonymous need no password and gain no capabilities (src/xfer.c
// check_login), but their nonce must still cover the body.
func TestVerifyLoginNobodyAndAnonymous(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)
	for _, login := range []string{"nobody", "anonymous"} {
		card := buildLoginCard(login, "whatever", code, []byte("pull sc pc\n"))
		u, err := VerifyLogin(d, code, card)
		if err != nil {
			t.Fatalf("VerifyLogin %s: %v", login, err)
		}
		if u.Login != login || u.Cap != "" {
			t.Fatalf("VerifyLogin %s = %+v, want login %q with no capabilities", login, u, login)
		}
		replay := onWire(&xfer.LoginCard{User: login, Nonce: card.Nonce, Signature: card.Signature},
			[]byte("push sc pc\n"))
		if _, err := VerifyLogin(d, code, replay); err != ErrAuthFailed {
			t.Fatalf("VerifyLogin replayed %s: got %v, want ErrAuthFailed", login, err)
		}
	}
}

// An account with no stored password cannot be logged into: its signature
// would be SHA1(nonce), which anyone can compute (src/xfer.c check_login
// requires length(pw)>0).
func TestVerifyLoginRefusesPasswordless(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)
	if _, err := d.Exec("INSERT INTO user(login, pw, cap) VALUES('carol', '', 'oi')"); err != nil {
		t.Fatalf("insert carol: %v", err)
	}
	payload := []byte("pull sc pc\n")
	nonce := sha1Hex(payload)
	card := onWire(&xfer.LoginCard{User: "carol", Nonce: nonce, Signature: sha1Hex([]byte(nonce))},
		payload)
	if _, err := VerifyLogin(d, code, card); err != ErrAuthFailed {
		t.Fatalf("VerifyLogin with empty password: got %v, want ErrAuthFailed", err)
	}
}

// developer and reader are capability templates other users inherit, not
// accounts, even when a row for them carries a password.
func TestVerifyLoginRefusesReservedLogins(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)
	for _, login := range []string{"developer", "reader"} {
		secret := sha1Hex([]byte(code + "/" + login + "/pw"))
		if _, err := d.Exec(
			// New repositories already hold these rows, as fossil's do; give
			// each a password so only the reserved-name rule can refuse it.
			"INSERT INTO user(login, pw, cap) VALUES(?, ?, 'oi')"+
				" ON CONFLICT(login) DO UPDATE SET pw=excluded.pw, cap=excluded.cap",
			login, secret); err != nil {
			t.Fatalf("insert %s: %v", login, err)
		}
		card := buildLoginCard(login, "pw", code, []byte("pull sc pc\n"))
		if _, err := VerifyLogin(d, code, card); err != ErrAuthFailed {
			t.Fatalf("VerifyLogin %s: got %v, want ErrAuthFailed", login, err)
		}
	}
}

// aliceCard creates alice with a password and returns a valid login card.
func aliceCard(t *testing.T, d *db.DB, code string) *xfer.LoginCard {
	t.Helper()
	if err := CreateUser(d, code, "alice", "secret123", "o"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return buildLoginCard("alice", "secret123", code, []byte("push sc pc\nigot abc123\n"))
}

// TestVerifyLoginIgnoresCookieExpiry: user.cexpire is the expiry of fossil's
// web-login cookie, which fossil's sync login never reads (#441). A Julian
// expiry fossil wrote, the 0 its logout writes, and an expiry long past all
// still log in.
func TestVerifyLoginIgnoresCookieExpiry(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)
	card := aliceCard(t, d, code)
	for _, cexpire := range []string{"julianday('now')+365", "0", "julianday('now')-1",
		"'2020-01-01 00:00:00'"} {
		if _, err := d.Exec("UPDATE user SET cexpire=" + cexpire + " WHERE login='alice'"); err != nil {
			t.Fatalf("set cexpire: %v", err)
		}
		if _, err := VerifyLogin(d, code, card); err != nil {
			t.Errorf("cexpire=%s: VerifyLogin = %v, want success", cexpire, err)
		}
	}
}

// TestVerifyLoginChecksInviteExpiry: an invite's expiry, kept apart from
// cexpire, refuses the login once past and admits it before, whatever
// cexpire holds. The expiry is an instant, so the zone it was computed in
// does not move it.
func TestVerifyLoginChecksInviteExpiry(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)
	card := aliceCard(t, d, code)
	east := time.FixedZone("UTC+10", 10*3600)
	west := time.FixedZone("UTC-6", -6*3600)
	cases := []struct {
		expiry  time.Time
		cexpire string
		want    error
	}{
		{time.Now().Add(time.Hour).In(east), "0", nil},
		{time.Now().Add(time.Hour).In(west), "julianday('now')-1", nil},
		{time.Now().Add(-time.Hour).In(east), "julianday('now')+365", ErrAuthFailed},
		{time.Now().Add(-time.Hour).In(west), "NULL", ErrAuthFailed},
	}
	for _, c := range cases {
		if err := SetInviteExpiry(d, "alice", c.expiry); err != nil {
			t.Fatalf("SetInviteExpiry: %v", err)
		}
		if _, err := d.Exec("UPDATE user SET cexpire=" + c.cexpire + " WHERE login='alice'"); err != nil {
			t.Fatalf("set cexpire: %v", err)
		}
		if _, err := VerifyLogin(d, code, card); err != c.want {
			t.Errorf("invite expiry %v, cexpire=%s: VerifyLogin = %v, want %v",
				c.expiry, c.cexpire, err, c.want)
		}
	}
}

// An invite expiry that does not read as a time refuses the login.
func TestVerifyLoginRefusesUnreadableInviteExpiry(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)
	card := aliceCard(t, d, code)
	if _, err := d.Exec("INSERT INTO config(name, value) VALUES(?, 'soon')",
		InviteExpiryKey("alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLogin(d, code, card); err != ErrAuthFailed {
		t.Errorf("VerifyLogin = %v, want ErrAuthFailed", err)
	}
}

// Deleting a user drops its invite expiry, so a later user of that login
// does not inherit it.
func TestDeleteUserDropsInviteExpiry(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)
	aliceCard(t, d, code)
	if err := SetInviteExpiry(d, "alice", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := DeleteUser(d, "alice"); err != nil {
		t.Fatal(err)
	}
	card := aliceCard(t, d, code)
	if _, err := VerifyLogin(d, code, card); err != nil {
		t.Errorf("VerifyLogin after re-creating alice = %v, want success", err)
	}
}
