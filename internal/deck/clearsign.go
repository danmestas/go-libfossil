package deck

import "bytes"

// A control artifact blob may be PGP- or SSH-clearsigned: the manifest text
// is wrapped in an armored header, a blank line, then the cards, and finally a
// detached signature block. Fossil's own src/manifest.c (remove_pgp_signature)
// strips this framing before verifying the Z-card checksum and parsing cards,
// so the checksum is computed over the inner content only. These are the exact
// marker strings it matches.
var (
	clearsignHeaders = [][]byte{
		[]byte("-----BEGIN PGP SIGNED MESSAGE-----"),
		[]byte("-----BEGIN SSH SIGNED MESSAGE-----"),
	}
	clearsignSignatures = [][]byte{
		[]byte("\n-----BEGIN PGP SIGNATURE-----"),
		[]byte("\n-----BEGIN SSH SIGNATURE-----"),
	}
)

// clearsignHeaderLen is the fixed byte length of every clearsign header marker
// above; the armor-header scan begins just past it. All markers share this
// length, which the assertion in stripClearsign pins.
const clearsignHeaderLen = 34

// stripClearsign returns the inner control-artifact content of a clearsigned
// blob -- the bytes between the armor header's blank-line separator and the
// trailing signature block -- which is what the Z card is computed over and
// what the card parser consumes. It mirrors fossil's remove_pgp_signature
// (src/manifest.c): a byte-for-byte identity on non-clearsigned input (the
// overwhelmingly common case) and idempotent, since stripped content no longer
// begins with a header marker. The artifact's content-addressed hash is taken
// over the raw blob elsewhere, so this stripping never changes which UUID an
// artifact resolves to.
func stripClearsign(data []byte) []byte {
	header := matchPrefix(data, clearsignHeaders)
	if header == nil {
		return data
	}
	if len(header) != clearsignHeaderLen {
		panic("deck.stripClearsign: clearsign header marker length mismatch")
	}

	// Skip the armor headers (e.g. "Hash: SHA1") up to and including the blank
	// line that separates them from the message body. i lands on the first
	// byte of the inner content.
	n := len(data)
	i := clearsignHeaderLen
	for i < n && !afterBlankLine(data, i) {
		i++
	}
	if i >= n {
		return data // No message body found; verifyZ will reject it.
	}
	body := data[i:]

	// Trim the trailing signature block. Scan backward for the '\n' that
	// begins "\n-----BEGIN PGP SIGNATURE-----", keeping that '\n' so the inner
	// content still ends in a newline (matching fossil's n = i+1).
	for j := len(body) - 1; j >= 0; j-- {
		if body[j] != '\n' {
			continue
		}
		if matchPrefix(body[j:], clearsignSignatures) != nil {
			return body[:j+1]
		}
	}
	return body // Header but no signature trailer; leave the body as-is.
}

// afterBlankLine reports whether index i in z sits immediately after a blank
// line, i.e. z[i-1] is a '\n' preceded by either another '\n' or a "\r\n".
// Mirrors fossil's after_blank_line. i is always >= clearsignHeaderLen at the
// call site, so the lookbehinds are in bounds; the guards assert that.
func afterBlankLine(z []byte, i int) bool {
	if i < 1 || z[i-1] != '\n' {
		return false
	}
	if i >= 2 && z[i-2] == '\n' {
		return true
	}
	if i >= 3 && z[i-2] == '\r' && z[i-3] == '\n' {
		return true
	}
	return false
}

// matchPrefix returns the first marker in markers that is a prefix of data, or
// nil if none match.
func matchPrefix(data []byte, markers [][]byte) []byte {
	for _, marker := range markers {
		if bytes.HasPrefix(data, marker) {
			return marker
		}
	}
	return nil
}
