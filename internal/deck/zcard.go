package deck

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
)

func VerifyZ(data []byte) error {
	if len(data) < 35 {
		return fmt.Errorf("deck.VerifyZ: manifest too short (%d bytes)", len(data))
	}
	tail := data[len(data)-35:]
	if tail[0] != 'Z' || tail[1] != ' ' || tail[34] != '\n' {
		return fmt.Errorf("deck.VerifyZ: invalid Z-card format")
	}
	stated := string(tail[2:34])
	computed := computeZ(data[:len(data)-35])
	if computed != stated {
		return fmt.Errorf("deck.VerifyZ: checksum mismatch: stated=%s computed=%s", stated, computed)
	}
	return nil
}

// parseZCard validates a Z card reached inside the artifact body. §4.5.1
// puts the Z card in neither occurrence class: it has no duplicate guard,
// because "exactly one Z, last" follows from the checksum framing rather
// than from a card rule, and an earlier Z line is ordinary content covered
// by the final checksum VerifyZ has already checked. So this carries no
// state and imposes no last-card rule.
//
// It is still a typed card, not an arbitrary-byte channel: §4.7.19 fixes
// the payload at exactly 32 hexadecimal characters, never decoded. Without
// the check we would accept and forward artifacts canonical fossil refuses.
func parseZCard(args string) error {
	token := strings.TrimSpace(args)
	if len(token) != 32 {
		return fmt.Errorf("Z-card checksum must be 32 characters, got %d", len(token))
	}
	for i := 0; i < len(token); i++ {
		if !isHexDigit(token[i]) {
			return fmt.Errorf("Z-card checksum is not hexadecimal: %q", token)
		}
	}
	return nil
}

// isHexDigit reports whether c is a base-16 digit in either case. §6.1
// defines the Z payload's grammar as md5-token = 32hexdig-ci, a
// parse-accept grammar that is explicitly case-insensitive, so rejecting
// uppercase would refuse artifacts canonical fossil accepts. The
// case-insensitivity is structural only: §6.2 still requires an uppercase
// token to fail verification, which VerifyZ enforces by comparing
// byte-exact against the lowercase computeZ.
func isHexDigit(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'f':
		return true
	case c >= 'A' && c <= 'F':
		return true
	default:
		return false
	}
}

func computeZ(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}
