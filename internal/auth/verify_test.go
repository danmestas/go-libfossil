package auth

import (
	"crypto/sha1"
	"encoding/hex"
	"testing"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// buildLoginCard constructs a LoginCard matching Fossil's protocol.
// payload is the raw xfer message bytes that the nonce is derived from.
func buildLoginCard(user, password, projectCode string, payload []byte) *xfer.LoginCard {
	nonce := sha1Hex(payload)
	sharedSecret := sha1Hex([]byte(projectCode + "/" + user + "/" + password))
	signature := sha1Hex([]byte(nonce + sharedSecret))
	return &xfer.LoginCard{User: user, Nonce: nonce, Signature: signature}
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
	payload := []byte("push\nigot abc123\n")
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
	payload := []byte("push\nigot abc123\n")
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
	payload := []byte("push\nigot abc123\n")
	card := buildLoginCard("doesnotexist", "anypassword", code, payload)

	// Verify should fail
	_, err := VerifyLogin(d, code, card)
	if err != ErrAuthFailed {
		t.Errorf("VerifyLogin unknown user: got %v, want ErrAuthFailed", err)
	}
}

func TestVerifyLoginExpired(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	// Create user
	err := CreateUser(d, code, "alice", "secret123", "o")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Set cexpire to past (2020-01-01)
	_, err = d.Exec("UPDATE user SET cexpire='2020-01-01 00:00:00' WHERE login='alice'")
	if err != nil {
		t.Fatalf("set cexpire: %v", err)
	}

	// Build valid login card
	payload := []byte("push\nigot abc123\n")
	card := buildLoginCard("alice", "secret123", code, payload)

	// Verify should fail due to expiration
	_, err = VerifyLogin(d, code, card)
	if err != ErrAuthFailed {
		t.Errorf("VerifyLogin expired: got %v, want ErrAuthFailed", err)
	}
}

func TestVerifyLoginNonExpired(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	// Create user
	err := CreateUser(d, code, "alice", "secret123", "ios")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Set cexpire to future (2030-01-01)
	_, err = d.Exec("UPDATE user SET cexpire='2030-01-01 00:00:00' WHERE login='alice'")
	if err != nil {
		t.Fatalf("set cexpire: %v", err)
	}

	// Build valid login card
	payload := []byte("push\nigot abc123\n")
	card := buildLoginCard("alice", "secret123", code, payload)

	// Verify should succeed
	user, err := VerifyLogin(d, code, card)
	if err != nil {
		t.Fatalf("VerifyLogin non-expired: expected success, got %v", err)
	}

	if user.Login != "alice" {
		t.Errorf("user.Login = %q, want alice", user.Login)
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
