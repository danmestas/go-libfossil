package merge

import "bytes"

// lineLimit is the longest line, in bytes without its newline, that
// fossil's line diff accepts (LENGTH_MASK in src/diff.c).
const lineLimit = 1<<15 - 1

// IsText reports whether fossil's line merge accepts data: it holds no NUL
// byte and no line longer than lineLimit (count_lines and break_into_lines
// in src/diff.c). Fossil refuses to line-merge when any of the three
// versions fails, and handles the file as binary instead. Unlike fossil,
// UTF-16 text is not converted first, so it reads as binary.
func IsText(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	for len(data) > 0 {
		n := bytes.IndexByte(data, '\n')
		if n < 0 {
			n = len(data)
		}
		if n > lineLimit {
			return false
		}
		data = data[min(n+1, len(data)):]
	}
	return true
}
