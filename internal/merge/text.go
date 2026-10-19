package merge

// lineLimit is the longest line, in bytes without its newline, that
// fossil's line diff accepts (LENGTH_MASK in src/diff.c).
const lineLimit = lengthMask

// IsText reports whether fossil's line merge accepts data: after the
// conversion text_diff applies (a UTF-8 BOM dropped, UTF-16 converted to
// UTF-8), it holds no NUL byte and no line longer than lineLimit
// (count_lines and break_into_lines in src/diff.c). Fossil refuses to
// line-merge when any of the three versions fails, and handles the file as
// binary instead.
func IsText(data []byte) bool {
	_, _, ok := breakIntoLines(toUTF8NoBOM(data))
	return ok
}
