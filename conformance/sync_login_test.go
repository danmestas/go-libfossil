package conformance

import (
	"strings"
)

// Login cards are built here from the draft's formulas (Sections 6.2 to
// 6.4), not by go-libfossil's own computeLogin, so a case cannot agree with
// the client by sharing its mistake.

// sharedSecret is SHA1(project-code "/" login "/" password), Section 6.3.
func sharedSecret(projectCode, login, password string) string {
	return sha1Hex(projectCode + "/" + login + "/" + password)
}

// signature is SHA1(NONCE-hex || shared-secret-hex), Section 6.4.
func signature(nonce, secret string) string {
	return sha1Hex(nonce + secret)
}

// sign replaces each "{LOGIN user password [how]}" line of body with a login
// card whose nonce covers every byte after that line (Section 6.2). Lines
// are signed from the last up, so an earlier login covers a later one. how
// changes one thing about an otherwise correct card:
//
//	upper   the signature in uppercase hex
//	zero    a signature of forty zeros
//	stale   a nonce over the body with one byte more, as when the body
//	        changed after it was signed
func (f *syncFixture) sign(body string) string {
	parts := strings.SplitAfter(body, "\n")
	for i := len(parts) - 1; i >= 0; i-- {
		line := strings.TrimSuffix(parts[i], "\n")
		if !strings.HasPrefix(line, "{LOGIN ") || !strings.HasSuffix(line, "}") {
			continue
		}
		args := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, "{LOGIN "), "}"))
		if len(args) != 2 && len(args) != 3 {
			panic("conformance: login placeholder " + line)
		}
		tail := strings.Join(parts[i+1:], "")
		how := ""
		if len(args) == 3 {
			how = args[2]
		}
		parts[i] = loginLine(f.vars["PC"], args[0], args[1], how, tail) + "\n"
	}
	return strings.Join(parts, "")
}

// loginLine is the login card for user over tail.
func loginLine(projectCode, user, password, how, tail string) string {
	nonce := sha1Hex(tail)
	if how == "stale" {
		nonce = sha1Hex(tail + "x")
	}
	sig := signature(nonce, sharedSecret(projectCode, user, password))
	switch how {
	case "", "stale":
	case "upper":
		sig = strings.ToUpper(sig)
	case "zero":
		sig = strings.Repeat("0", 40)
	default:
		panic("conformance: unknown login change " + how)
	}
	return "login " + fossilize(user) + " " + nonce + " " + sig
}
