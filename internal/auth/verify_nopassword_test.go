package auth

import (
	"errors"
	"testing"

	"github.com/danmestas/go-libfossil/internal/xfer"
)

// An account whose stored password is empty cannot be logged into: with no
// secret, SHA1(nonce) alone would pass the signature check. fossil's
// check_login selects only rows with length(pw)>0 (src/xfer.c), and a new
// repository's creator starts with exactly such an account, holding Setup.
func TestVerifyLoginRejectsEmptyStoredPassword(t *testing.T) {
	d := setupTestDB(t)
	code := projectCode(t, d)

	nonce := sha1Hex([]byte("push\n"))
	card := &xfer.LoginCard{User: "admin", Nonce: nonce, Signature: sha1Hex([]byte(nonce))}
	if _, err := VerifyLogin(d, code, card); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("VerifyLogin with no stored password: err = %v, want ErrAuthFailed", err)
	}
}
