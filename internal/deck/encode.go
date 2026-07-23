package deck

import "strings"

// FossilEncode escapes a string for use as an artifact/sync card field value,
// per §4.3 of the format spec. It mirrors canonical fossil's fossilize(): the
// eight bytes newline, space, tab, carriage return, vertical tab, form feed,
// NUL and backslash are each written as a two-character backslash escape; all
// other bytes pass through unchanged.
func FossilEncode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			b.WriteString(`\n`)
		case ' ':
			b.WriteString(`\s`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case 0:
			b.WriteString(`\0`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// FossilDecode reverses FossilEncode, per §4.3. It mirrors canonical fossil's
// defossilize(): each recognized escape maps back to its raw byte, and an
// unrecognized escape drops the backslash and keeps the following byte literally.
// A lone trailing backslash (which a conforming encoder never emits) is left
// as-is rather than consuming a nonexistent following byte.
func FossilDecode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 's':
			b.WriteByte(' ')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'v':
			b.WriteByte('\v')
		case 'f':
			b.WriteByte('\f')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		default:
			// §4.3: an unrecognized escape drops the backslash and keeps the byte.
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
