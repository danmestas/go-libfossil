package fsltype

import (
	"database/sql"
	"strings"
)

// The event comments fossil derives are built with its own printf
// conversions (src/printf.c): %h escapes HTML, %t encodes a URL query value,
// %S and %!S shorten a hash for display and for a link. These reproduce them
// byte for byte, so a comment go-libfossil derives equals the one fossil
// derives from the same artifact.

// hashNameMax is fossil's HNAME_MAX, the length of a SHA3-256 hash.
const hashNameMax = 64

// HashDigits returns the hash lengths fossil's %S and %!S conversions use:
// the human length (the hash-digits setting, default 10, clamped to 6..64)
// and the url length (human+6, floored at 16, capped at 64). Mirrors
// fossil's hash_digits(), which reads the setting from the repository and,
// failing that, the user's global configuration; only the repository is
// visible here.
func HashDigits(q interface {
	QueryRow(query string, args ...any) *sql.Row
}) (human, url int) {
	human = 10
	var v int
	if q.QueryRow("SELECT value FROM config WHERE name='hash-digits'").Scan(&v) == nil && v != 0 {
		human = v
	}
	human = min(max(human, 6), hashNameMax)
	url = min(max(human+6, 16), hashNameMax)
	return human, url
}

// HashPrefix truncates a hash for display, matching fossil's %S and %!S
// conversions: shorter values pass through unchanged.
func HashPrefix(hash string, n int) string {
	if len(hash) > n {
		return hash[:n]
	}
	return hash
}

// HTMLize escapes exactly what fossil's %h conversion escapes, in the same
// forms. Go's html.EscapeString agrees on every character except the double
// quote, which it writes as &#34; where fossil writes &quot;.
func HTMLize(s string) string {
	return fossilHTML.Replace(s)
}

var fossilHTML = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)

// HTTPize encodes s as fossil's %t conversion does (httpize in
// src/encode.c): ASCII letters, digits and ".$~-_" pass through, a space
// becomes '+', and every other byte, '/' included, becomes %HH.
func HTTPize(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte(".$~-_", c) >= 0:
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}
