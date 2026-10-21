package merge

import "bytes"

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// toUTF8NoBOM is fossil's blob_to_utf8_no_bom(p, 0), which text_diff applies
// to both inputs before diffing them, so the merge sees (and writes) the
// converted text. A UTF-8 BOM is dropped, and the text is cut at its first
// NUL as fossil's string copy does. Text with a UTF-16 BOM is converted to
// UTF-8. Anything else is returned as is. data is never modified.
func toUTF8NoBOM(data []byte) []byte {
	if bytes.HasPrefix(data, utf8BOM) {
		rest := data[len(utf8BOM):]
		if nul := bytes.IndexByte(rest, 0); nul >= 0 {
			rest = rest[:nul]
		}
		return append([]byte{}, rest...)
	}
	reverse, ok := utf16BOM(data)
	if !ok {
		return data
	}
	buf := append([]byte{}, data...)
	if reverse {
		// Fossil swaps byte pairs from the end, so an odd-length text keeps
		// its first byte in place.
		for i := len(buf); i > 1; i -= 2 {
			buf[i-1], buf[i-2] = buf[i-2], buf[i-1]
		}
	}
	return utf16LEToUTF8(append(buf, 0, 0, 0)[2:])
}

// utf16BOM is fossil's starts_with_utf16_bom as it runs on a little-endian
// machine: FF FE is a BOM in native order, FE FF a reversed one, and either
// followed by two NULs is UTF-32, not UTF-16.
func utf16BOM(data []byte) (reverse, ok bool) {
	if len(data) < 2 || (len(data) >= 4 && data[2] == 0 && data[3] == 0) {
		return false, false
	}
	switch {
	case data[0] == 0xff && data[1] == 0xfe:
		return false, true
	case data[0] == 0xfe && data[1] == 0xff:
		return true, true
	}
	return false, false
}

// utf16LEToUTF8 converts little-endian UTF-16, up to its first U+0000, the
// way SQLite binds and reads it (fossil converts through SQLite): a second
// BOM is dropped and sets the byte order, a surrogate takes the next unit as
// its partner without checking it, and a surrogate with no next unit is
// written as is.
func utf16LEToUTF8(z []byte) []byte {
	end := 0
	for end+1 < len(z) && (z[end] != 0 || z[end+1] != 0) {
		end += 2
	}
	z = z[:end]
	bigEndian := false
	if len(z) > 1 && (z[0] == 0xfe && z[1] == 0xff || z[0] == 0xff && z[1] == 0xfe) {
		bigEndian = z[0] == 0xfe
		z = z[2:]
	}
	unit := func(i int) int {
		if bigEndian {
			return int(z[i])<<8 | int(z[i+1])
		}
		return int(z[i]) | int(z[i+1])<<8
	}
	out := make([]byte, 0, len(z)*2)
	for i := 0; i < len(z); {
		c := unit(i)
		i += 2
		if c >= 0xd800 && c < 0xe000 && i < len(z) {
			c2 := unit(i)
			i += 2
			c = (c2 & 0x03ff) + (c&0x003f)<<10 + ((c&0x03c0)+0x0040)<<10
		}
		out = appendUTF8(out, c)
	}
	return out
}

// appendUTF8 is SQLite's WRITE_UTF8, which encodes surrogates like any other
// code point (Go's encoder would replace them).
func appendUTF8(out []byte, c int) []byte {
	switch {
	case c < 0x80:
		return append(out, byte(c))
	case c < 0x800:
		return append(out, 0xc0+byte(c>>6&0x1f), 0x80+byte(c&0x3f))
	case c < 0x10000:
		return append(out, 0xe0+byte(c>>12&0x0f), 0x80+byte(c>>6&0x3f), 0x80+byte(c&0x3f))
	}
	return append(out, 0xf0+byte(c>>18&0x07), 0x80+byte(c>>12&0x3f),
		0x80+byte(c>>6&0x3f), 0x80+byte(c&0x3f))
}
