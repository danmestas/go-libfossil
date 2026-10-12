package deck

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
)

// VerifyZ validates a control artifact's trailing Z-card checksum. The blob
// may arrive PGP/SSH clearsigned (historical artifacts, e.g. Fossil's own
// 2007-era check-ins); stripClearsign isolates the inner content the Z card
// is computed over so the last-35-byte check lands on the Z card and not on a
// trailing signature block. The artifact's content-addressed hash is computed
// over the raw blob elsewhere and is unaffected by this stripping.
func VerifyZ(data []byte) error {
	return verifyZ(stripClearsign(data))
}

// verifyZ checks the Z card of already-clearsign-stripped artifact content.
// It mirrors fossil's verify_z_card (src/manifest.c): the Z card is located
// as the last 35 bytes beginning "Z ", the checksum covers every byte before
// it, and the stated digest is the 32 bytes at n-33 -- byte n-1, the final
// LF, is never read. §6.2: a body too short to hold a Z card, or one whose
// last 35 bytes are not a Z card, has the Z card absent, which this reports
// as success and leaves the required-card rule (§5.2) to judge; only a
// present-but-wrong checksum is an error here. Framing (§4.1) -- the missing
// final LF, residue after the Z card -- is the caller's to check, so this
// never rejects an input the rule it breaks should.
func verifyZ(body []byte) error {
	n := len(body)
	if n < 35 {
		return nil // no room for a Z card: absent
	}
	if body[n-35] != 'Z' || body[n-34] != ' ' {
		return nil // last 35 bytes are not a Z card: absent
	}
	// §6.2 compares byte-exact against the lowercase computeZ, so an
	// uppercase digest fails verification even though §6.1's grammar admits
	// it. body[n-1] is deliberately not read.
	stated := body[n-33 : n-1]
	computed := computeZ(body[:n-35])
	if computed != string(stated) {
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
