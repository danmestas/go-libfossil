package sync

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/danmestas/go-libfossil/internal/xfer"
	"github.com/danmestas/go-libfossil/simio"
)

// computeLogin produces a LoginCard for the given credentials.
// payload is the encoded bytes of all non-login cards (including random comment).
func computeLogin(user, password, projectCode string, payload []byte) *xfer.LoginCard {
	if user == "" {
		panic("sync.computeLogin: user must not be empty")
	}
	if projectCode == "" {
		panic("sync.computeLogin: projectCode must not be empty")
	}
	if payload == nil {
		panic("sync.computeLogin: payload must not be nil")
	}
	nonce := sha1Hex(payload)
	sharedSecret := sha1Hex([]byte(projectCode + "/" + user + "/" + password))
	signature := sha1Hex([]byte(nonce + sharedSecret))
	return &xfer.LoginCard{User: user, Nonce: nonce, Signature: signature}
}

// appendRandomComment appends "# <random-hex>\n" to payload for nonce uniqueness.
func appendRandomComment(payload []byte, rng simio.Rand) []byte {
	rb := make([]byte, 20)
	rng.Read(rb)
	comment := fmt.Sprintf("# %s\n", hex.EncodeToString(rb))
	return append(payload, []byte(comment)...)
}

func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}
