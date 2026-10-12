package deck

import (
	"strings"
	"unicode/utf8"
)

// The §4.6 name predicates, ported from fossil so the parser accepts and
// rejects exactly the names fossil does. They operate on the decoded name,
// since that is the form the manifest's sort order and the repository's
// pathname rules are defined over.

// simplePathname reports whether z is a simple pathname per §4.6.1, matching
// fossil's file_is_simple_pathname_nonstrict (src/file.c): a non-empty name
// with no leading '/', no '//' run, no trailing '/', and no "." or ".." path
// element. Unlike the strict form it says nothing about backslashes or UTF-8
// validity, so a name carrying those bytes is still a simple pathname -- the
// writer, not the parser, is where fossil enforces the stricter rule.
func simplePathname(z string) bool {
	if z == "" || z[0] == '/' {
		return false
	}
	if dotElementAt(z, 0) {
		return false
	}
	for i := 0; i < len(z); i++ {
		if z[i] != '/' {
			continue
		}
		if i+1 >= len(z) || z[i+1] == '/' { // trailing slash or "//"
			return false
		}
		if dotElementAt(z, i+1) {
			return false
		}
	}
	return true
}

// StrictPathname reports whether z is a simple pathname as fossil's
// file_is_simple_pathname(z, 1) decides it (src/file.c), the rule a uvfile
// NAME must meet: a simple pathname with no backslash, in strict UTF-8 with no
// character above U+FFFF, no private-use character (U+E000..U+F8FF), no
// noncharacter (U+FDD0..U+FDEF, U+FFFE, U+FFFF) and no surrogate.
func StrictPathname(z string) bool {
	if !simplePathname(z) || strings.IndexByte(z, '\\') >= 0 {
		return false
	}
	for i := 0; i < len(z); {
		r, n := utf8.DecodeRuneInString(z[i:])
		if r == utf8.RuneError && n <= 1 {
			return false // invalid or overlong UTF-8, or a surrogate
		}
		if r > 0xFFFF || (r >= 0xE000 && r <= 0xF8FF) || (r >= 0xFDD0 && r <= 0xFDEF) ||
			r >= 0xFFFE {
			return false
		}
		i += n
	}
	return true
}

// dotElementAt reports whether the path element beginning at index start is
// exactly "." or "..", i.e. one or two dots terminated by '/' or end of string.
func dotElementAt(z string, start int) bool {
	if start >= len(z) || z[start] != '.' {
		return false
	}
	if start+1 >= len(z) || z[start+1] == '/' { // "."
		return true
	}
	if z[start+1] == '.' && (start+2 >= len(z) || z[start+2] == '/') { // ".."
		return true
	}
	return false
}

// wikiNameWellformed reports whether z is a well-formed wiki name per §4.6.2,
// matching fossil's wiki_name_is_wellformed (src/wiki.c): 1 to 100 bytes, no
// leading or trailing space, no interior control byte (which includes tab and
// newline), and no two spaces in a row. Bytes 0x20 and below are the only ones
// it rejects, so a byte from 0x7F up -- DEL, or an invalid UTF-8 byte -- is
// allowed.
func wikiNameWellformed(z string) bool {
	if z == "" || z[0] <= 0x20 {
		return false
	}
	for i := 1; i < len(z); i++ {
		if z[i] < 0x20 {
			return false
		}
		if z[i] == 0x20 && z[i-1] == 0x20 {
			return false
		}
	}
	if z[len(z)-1] == ' ' || len(z) > 100 {
		return false
	}
	return true
}

// fileIsReservedName reports whether name is one of fossil's reserved
// repository filenames, matching file_is_reserved_name (src/file.c): _FOSSIL_
// or .fslckout, in either case, optionally with a -wal, -shm or -journal
// suffix, either at the root or as the last component of a path. §4.6.3: such
// an F-card is silently dropped from a manifest after the sort-order check,
// not rejected.
func fileIsReservedName(name string) bool {
	n := len(name)
	if n < 8 { // len("_FOSSIL_")
		return false
	}
	end := n // base name ends just before here, once any suffix is stripped
	gotSuffix := false
	if n >= 12 { // len("_FOSSIL_-shm")
		switch {
		case name[end-4] == '-':
			if s := name[end-3 : end]; !strings.EqualFold(s, "wal") && !strings.EqualFold(s, "shm") {
				return false
			}
			end -= 4
			gotSuffix = true
		case n >= 16 && name[end-8] == '-': // len("_FOSSIL_-journal")
			if !strings.EqualFold(name[end-7:end], "journal") {
				return false
			}
			end -= 8
			gotSuffix = true
		}
	}
	switch name[end-1] {
	case '_':
		if end < 8 || !strings.EqualFold(name[end-8:end], "_FOSSIL_") {
			return false
		}
		if end == 8 {
			return true
		}
		return name[end-9] == '/' || gotSuffix
	case 'T', 't':
		if end < 9 || name[end-9] != '.' || !strings.EqualFold(name[end-9:end], ".fslckout") {
			return false
		}
		if end == 9 {
			return true
		}
		return name[end-10] == '/' || gotSuffix
	default:
		return false
	}
}
